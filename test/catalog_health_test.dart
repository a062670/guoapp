import 'package:duanju_app/catalog_browser.dart';
import 'package:duanju_app/core_bridge.dart';
import 'package:duanju_app/models.dart';
import 'package:duanju_app/source_health.dart';
import 'package:flutter_test/flutter_test.dart';

import 'fixtures.dart';

/// 目录刷新接入健康度账本后的行为回归。
///
/// 三个约束（踩中任何一个用户都会觉得「源丢了」）：
///   1. 连续失败的源在冷却期内不再发请求，但它上次的条目仍然显示；
///   2. force（下拉刷新 / 手动重试）必须真的重试，不能继续跳过；
///   3. 健康源先发请求，死源排到最后，不占前排并发槽位。

class _HealthRepository extends FixtureRepository {
  _HealthRepository({this.failing = const {}});

  final Set<String> failing;

  @override
  Future<CatalogPage> catalog(
    String source, {
    int page = 1,
    String query = '',
    String category = '',
    bool force = false,
  }) async {
    requests.add(source);
    if (failing.contains(source)) throw AppFailure('模拟站点超时');
    return CatalogPage([
      Drama(
        id: '$source:$page',
        source: source,
        title: '$source 的条目',
        category: '合成数据',
      ),
    ], page: page);
  }
}

SourceGroup _groupOf(List<String> keys) => SourceGroup(
  'all',
  '全部站源',
  [for (final key in keys) SourceSite('libretv:$key', '源$key', '测试')],
);

void main() {
  test('连续失败的源会被跳过，但保留上次的条目', () async {
    final repository = _HealthRepository(failing: {'libretv:dead'});
    final health = SourceHealthBook();
    final browser = CatalogBrowser(repository, health: health);
    final group = _groupOf(['live', 'dead']);

    // 前两次照常请求（失败计数 0 → 1 → 2），并记住成功源的内容。
    for (var attempt = 0; attempt < 2; attempt++) {
      final result = await browser.load(group, force: true);
      expect(result.items.any((item) => item.source == 'libretv:dead'), isFalse,
          reason: '失败的源本来就没有条目');
    }
    expect(health.healthOf('libretv:dead')!.failures, 2);

    // 第三次：冷却期内，不再请求死源。
    repository.requests.clear();
    final skipped = await browser.load(group);
    expect(repository.requests, ['libretv:live']);
    expect(skipped.warning, contains('已暂时跳过'));
    expect(
      skipped.items.map((item) => item.source).toSet(),
      {'libretv:live'},
    );
  });

  test('force 刷新必须重试被跳过的源', () async {
    final repository = _HealthRepository(failing: {'libretv:dead'});
    final health = SourceHealthBook();
    final browser = CatalogBrowser(repository, health: health);
    final group = _groupOf(['live', 'dead']);
    await browser.load(group);
    await browser.load(group);
    expect(health.shouldSkip('libretv:dead'), isTrue);

    repository.requests.clear();
    await browser.load(group, force: true);
    expect(repository.requests, contains('libretv:dead'));
  });

  test('单源分组永远不跳过（用户就是冲着这个源来的）', () async {
    final repository = _HealthRepository();
    final health = SourceHealthBook();
    final browser = CatalogBrowser(repository, health: health);
    final group = _groupOf(['only']);
    // 先人为把该源标成连续失败。
    health.record('libretv:only', ok: false, elapsedMs: 0);
    health.record('libretv:only', ok: false, elapsedMs: 0);
    expect(health.shouldSkip('libretv:only'), isTrue);

    final result = await browser.load(group);
    expect(repository.requests, ['libretv:only']);
    expect(result.items.single.source, 'libretv:only');
  });

  test('健康源先请求，死源排在队尾（并发槽位优先给能出内容的源）', () async {
    final repository = _HealthRepository();
    final health = SourceHealthBook();
    health.record('libretv:slow', ok: true, elapsedMs: 900);
    health.record('libretv:fast', ok: true, elapsedMs: 50);
    final browser = CatalogBrowser(repository, health: health);

    await browser.load(_groupOf(['slow', 'unknown', 'fast']));
    expect(repository.requests, ['libretv:fast', 'libretv:slow', 'libretv:unknown']);
  });

  test('成功的源会重置失败计数，下一轮不再跳过', () async {
    final health = SourceHealthBook();
    final dead = _HealthRepository(failing: {'libretv:a'});
    final browser = CatalogBrowser(dead, health: health);
    final group = _groupOf(['a', 'b']);
    await browser.load(group);
    await browser.load(group, force: true);
    expect(health.shouldSkip('libretv:a'), isTrue);

    // 源恢复：手动重试成功后立刻解除跳过。
    final alive = _HealthRepository();
    final retry = CatalogBrowser(alive, health: health);
    await retry.load(group, force: true);
    expect(health.shouldSkip('libretv:a'), isFalse);
  });
}
