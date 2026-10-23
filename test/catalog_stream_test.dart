import 'package:duanju_app/catalog_browser.dart';
import 'package:duanju_app/core_bridge.dart';
import 'package:duanju_app/libretv.dart';
import 'package:duanju_app/models.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fixtures.dart';

/// 流式刷新的时序回归。
///
/// 「全部站源」可能包含几十个源、每个源一次网络请求。原来的实现是等**所有**
/// 源都回来才返回结果，慢的源会把快的源一起拖住（用户看到长时间空白）。
/// 现在每有一个源回来就推一次快照，首个源立刻可见、其余陆续补齐。
///
/// 这里用可控延迟的假仓库验证三件事：
///   1. 首个源返回后就让 UI 拿到部分结果（不等最慢的源）；
///   2. 部分结果只会增长，不会因为某个源失败而回退；
///   3. 单个源失败不影响其他源出内容。

class _DelayRepository extends FixtureRepository {
  _DelayRepository(this.delays, {this.failing = const {}});

  /// 每个源的模拟延迟（毫秒）。
  final Map<String, int> delays;

  /// 这些源直接抛错（模拟站点挂掉）。
  final Set<String> failing;

  @override
  Future<CatalogPage> catalog(
    String source, {
    int page = 1,
    String query = '',
    String category = '',
    bool force = false,
  }) async {
    await Future<void>.delayed(Duration(milliseconds: delays[source] ?? 0));
    if (failing.contains(source)) throw AppFailure('模拟站点超时');
    // 每个源贡献一条独立条目，便于观察「逐个补齐」。
    return CatalogPage(
      [
        Drama(
          id: '$source:item-$page',
          source: source,
          title: '$source 的${query.isEmpty ? '' : '搜索结果 '}$page',
          category: '合成数据',
        ),
      ],
      page: page,
      hasMore: false,
    );
  }
}

SourceGroup _groupOf(List<String> keys) => SourceGroup(
  'all',
  '全部站源',
  [
    for (final key in keys)
      SourceSite('libretv:$key', '源$key', '测试'),
  ],
);

void main() {
  test('首个源返回就先渲染，不等最慢的源', () async {
    final repository = _DelayRepository({
      'libretv:fast': 10,
      'libretv:mid': 120,
      'libretv:slow': 260,
    });
    final browser = CatalogBrowser(repository);
    final partials = <int>[];
    final partialAt = <int>[];
    final watch = Stopwatch()..start();

    final result = await browser.load(
      _groupOf(['fast', 'mid', 'slow']),
      force: true,
      onPartial: (page) {
        partials.add(page.items.length);
        partialAt.add(watch.elapsedMilliseconds);
      },
    );
    watch.stop();

    // 首个源回来后必须已经推过一次（partial 存在且只含 1 条）。
    expect(partials, isNotEmpty, reason: '首个源返回后应立即推一次部分结果');
    expect(partials.first, 1, reason: '首次部分结果只应包含最快那个源');
    expect(
      partialAt.first < watch.elapsedMilliseconds ~/ 2,
      isTrue,
      reason:
          '首次推送应在总耗时的前半段（实际 ${partialAt.first}ms / '
          '${watch.elapsedMilliseconds}ms）——否则等于没流式',
    );
    // 部分结果只增不减。
    for (var i = 1; i < partials.length; i++) {
      expect(
        partials[i] >= partials[i - 1],
        isTrue,
        reason: '部分结果不应回退：$partials',
      );
    }
    // 最终结果包含全部三个源。
    expect(result.items.length, 3);
    expect(result.items.map((d) => d.source).toSet(), {
      'libretv:fast',
      'libretv:mid',
      'libretv:slow',
    });
  });

  test('单个源失败不影响其他源出内容', () async {
    final repository = _DelayRepository(
      {'libretv:a': 5, 'libretv:b': 20, 'libretv:c': 40},
      failing: {'libretv:b'},
    );
    final browser = CatalogBrowser(repository);
    final partials = <int>[];

    final result = await browser.load(
      _groupOf(['a', 'b', 'c']),
      force: true,
      onPartial: (page) => partials.add(page.items.length),
    );

    // 挂掉的那个源不该挡住另外两个。
    expect(result.items.length, 2);
    expect(result.warning, contains('模拟站点超时'));
    expect(partials, isNotEmpty);
  });

  test('搜索同样流式推送', () async {
    final repository = _DelayRepository({
      'libretv:s1': 10,
      'libretv:s2': 200,
    });
    final browser = CatalogBrowser(repository);
    final partials = <int>[];
    final watch = Stopwatch()..start();

    final result = await browser.load(
      _groupOf(['s1', 's2']),
      query: '测试',
      force: true,
      onPartial: (page) {
        partials.add(page.items.length);
      },
    );
    watch.stop();

    expect(partials, isNotEmpty, reason: '搜索也应在首个源返回后先出结果');
    expect(partials.first, 1);
    expect(result.items.length, 2);
  });

  test('缓存阶段同样先渲染', () async {
    final repository = _DelayRepository({'libretv:c1': 0, 'libretv:c2': 0});
    repository.cachedPages['libretv:c1'] = CatalogPage([
      const Drama(id: 'libretv:c1:old', source: 'libretv:c1', title: '缓存条目'),
    ]);
    final browser = CatalogBrowser(repository);
    final partials = <int>[];

    await browser.load(
      _groupOf(['c1', 'c2']),
      useCache: true,
      onPartial: (page) => partials.add(page.items.length),
    );

    expect(partials, isNotEmpty, reason: '缓存命中时也应先渲染，不等网络');
    expect(partials.first, 1);
  });


  test('先出的内容位置不动：新源只往后追加，不把已有条目挤下去', () async {
    // 请求顺序故意和到达顺序相反：慢源排在前面。若按请求顺序拼列表，
    // 慢源晚到会把已经显示的 fast / mid 整体往下挤（每批一次重排）。
    final repository = _DelayRepository({
      'libretv:slow': 260,
      'libretv:mid': 130,
      'libretv:fast': 10,
    });
    final browser = CatalogBrowser(repository);
    final partialIds = <List<String>>[];
    final watch = Stopwatch()..start();

    final result = await browser.load(
      _groupOf(['slow', 'mid', 'fast']),
      force: true,
      onPartial: (page) {
        partialIds.add([for (final item in page.items) item.id]);
      },
    );
    watch.stop();

    expect(partialIds.length, greaterThanOrEqualTo(2), reason: '应有多次流式推送');
    // 每次推送都必须以上一次的内容为前缀（只追加、不重排）。
    for (var index = 1; index < partialIds.length; index++) {
      final previous = partialIds[index - 1];
      final current = partialIds[index];
      expect(
        current.take(previous.length).toList(),
        previous,
        reason: '第 $index 次推送把已有条目挪了位置：$previous -> $current',
      );
    }
    // 先加载的源排在前面（按到达顺序，而不是请求顺序）。
    expect(result.items.map((item) => item.source).toList(), [
      'libretv:fast',
      'libretv:mid',
      'libretv:slow',
    ]);
    expect(partialIds.first.first, 'libretv:fast:item-1');
  });


  test('翻页的新条目追加在末尾，不会插回该源前面', () async {
    final repository = _PagedRepository();
    final browser = CatalogBrowser(repository);
    final group = _groupOf(['a', 'b']);

    final first = await browser.load(group, force: true);
    expect(first.items.map((item) => item.id).toList(), [
      'libretv:a:1:0',
      'libretv:a:1:1',
      'libretv:b:1:0',
    ]);

    final more = await browser.load(group, more: true);
    // 第一页的顺序原样保留，第二页整体接在后面（含先返回的源）。
    expect(more.items.map((item) => item.id).toList(), [
      'libretv:a:1:0',
      'libretv:a:1:1',
      'libretv:b:1:0',
      'libretv:a:2:0',
      'libretv:a:2:1',
      'libretv:b:2:0',
    ]);
  });
}

class _PagedRepository extends FixtureRepository {
  @override
  Future<CatalogPage> catalog(
    String source, {
    int page = 1,
    String query = '',
    String category = '',
    bool force = false,
  }) async => CatalogPage(
    [
      for (var index = 0; index < (source.endsWith(':a') ? 2 : 1); index++)
        Drama(
          id: '$source:$page:$index',
          source: 'libretv:$source',
          title: '$source-$page-$index',
        ),
    ],
    page: page,
    hasMore: page < 2,
  );
}
