import 'dart:async';
import 'dart:convert';

import 'package:duanju_app/core_bridge.dart';
import 'package:duanju_app/drpy.dart';
import 'package:duanju_app/libretv.dart';
import 'package:duanju_app/local_profiles.dart';
import 'package:duanju_app/models.dart';
import 'package:duanju_app/source_status.dart';
import 'package:duanju_app/sources_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';

import 'fixtures.dart';

class SourceFixtureRepository extends FixtureRepository {
  final statuses = <String, SourceStatus>{};
  final operations = <String>[];
  final statusRequests = <String>[];
  Completer<SourceStatus>? pending;

  /// LibreTV 导入桩：libretvSites 返回的列表即原生核心 libretv.json 内容。
  List<LibreTVSite> libretvSitesResult = const [];
  final libretvImports = <String>[];
  final libretvRemovals = <String>[];
  bool libretvFail = false;

  /// 混合订阅里附带的 drpy 脚本候选（type≥3 .js），导入结果透传。
  List<DrpyCandidate> libretvDrpyCandidates = const [];

  @override
  bool get supportsSourceManagement => true;

  @override
  Future<SourceStatus> sourceStatus(String source) async {
    statusRequests.add(source);
    return statuses[source] ??
        SourceStatus.fromJson({
          'source': source,
          'count': 3,
          'page': 2,
          'hasMore': true,
        });
  }

  @override
  Future<SourceStatus> startSourceJob(
    String source,
    String operation, {
    Drama? drama,
  }) async {
    operations.add('$source:$operation');
    if (pending != null) return pending!.future;
    return statuses[source] = SourceStatus.fromJson({
      'source': source,
      'count': 3,
      'running': true,
      'operation': operation,
      'stage': '查找新剧',
    });
  }

  @override
  Future<SourceStatus> cancelSourceJob(String source) async {
    operations.add('$source:cancel');
    return statuses[source] = SourceStatus.fromJson({
      'source': source,
      'count': 3,
      'running': false,
      'stage': '已停止',
    });
  }

  @override
  Future<List<LibreTVSite>> libretvSites() async => libretvSitesResult;

  @override
  Future<LibreTVImportResult> libretvImport(String subscription) async {
    if (libretvFail) throw AppFailure('配置解析失败，请确认是 LibreTV/TVBox 格式的 JSON');
    libretvImports.add(subscription);
    final site = LibreTVSite(key: 'new456', name: '新采集站', api: subscription);
    libretvSitesResult = [...libretvSitesResult, site];
    return LibreTVImportResult(
      added: [site],
      skipped: const [],
      drpy: libretvDrpyCandidates,
      verified: 1,
    );
  }

  @override
  Future<void> libretvRemove(String key) async {
    libretvRemovals.add(key);
    libretvSitesResult = libretvSitesResult
        .where((site) => site.key != key)
        .toList();
  }

  /// LibreTV 解析方式桩：默认「直链/嗅探」，测试可按需注入 parses。
  LibreTVParseConfig parseConfigResult = const LibreTVParseConfig();
  final parseSelections = <String>[];

  @override
  Future<LibreTVParseConfig> libretvParseConfig() async => parseConfigResult;

  @override
  Future<void> libretvSelectParse(String url) async {
    parseSelections.add(url);
    parseConfigResult = LibreTVParseConfig(
      parses: parseConfigResult.parses,
      selected: url,
    );
  }

  /// drpy 脚本源导入桩：drpySites 返回的列表即原生核心 drpy.json 内容。
  List<DrpySite> drpySitesResult = const [];
  final drpyImports = <String>[];
  final drpyRemovals = <String>[];
  bool drpyFail = false;

  @override
  Future<List<DrpySite>> drpySites() async => drpySitesResult;

  @override
  Future<DrpyImportResult> drpyImport(String script) async {
    if (drpyFail) throw AppFailure('未找到 drpy 脚本（需要包含 var rule 定义的 JS）');
    drpyImports.add(script);
    final site = DrpySite(key: 'rule123', name: '演示脚本源');
    // 与原生 drpy.json 存储一致：按 key upsert，重复导入不累积。
    drpySitesResult = [
      for (final existing in drpySitesResult)
        if (existing.key != site.key) existing,
      site,
    ];
    return DrpyImportResult(key: site.key, name: site.name);
  }

  @override
  Future<void> drpyRemove(String key) async {
    drpyRemovals.add(key);
    drpySitesResult = drpySitesResult.where((site) => site.key != key).toList();
  }
}

void main() {
  test('libretv site json keeps the collection type and defaults to json', () {
    final xml = LibreTVSite.fromJson(const {
      'key': 'xml',
      'name': 'XML 站',
      'api': 'https://a.example.com/at/xml/',
      'type': 2,
      'count': 7,
    });
    expect(xml.type, 2);
    expect(xml.isXml, isTrue);
    expect(xml.typeLabel, 'XML 采集');
    expect(xml.count, 7);
    // 旧订阅文件没有 type 字段（反序列化为 0），按 JSON 采集处理。
    final legacy = LibreTVSite.fromJson(const {
      'key': 'json',
      'name': 'JSON 站',
      'api': 'https://b.example.com/provide/vod',
    });
    expect(legacy.type, 1);
    expect(legacy.isXml, isFalse);
    expect(legacy.typeLabel, 'JSON 采集');
  });

  testWidgets('source list keeps bottom content above system navigation', (
    tester,
  ) async {
    tester.view.physicalSize = const Size(390, 844);
    tester.view.devicePixelRatio = 1;
    tester.view.padding = const FakeViewPadding(bottom: 34);
    addTearDown(tester.view.reset);
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    await tester.pumpWidget(
      MaterialApp(
        home: SourcesScreen(
          repository: SourceFixtureRepository(),
          store: store,
        ),
      ),
    );
    await tester.pumpAndSettle();
    final list = tester.widget<ListView>(find.byType(ListView));
    expect((list.padding! as EdgeInsets).bottom, 50);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });

  testWidgets(
    'source update prevents duplicate submits and can stop without losing cache',
    (tester) async {
      SharedPreferences.setMockInitialValues({});
      final store = testStore(await SharedPreferences.getInstance());
      // 管理员默认可见全部站源；显式启用密码锁以覆盖解锁后的站源范围。
      await store.enableSourceGate('666666');
      final repository = SourceFixtureRepository()
        ..pending = Completer<SourceStatus>();
      await tester.pumpWidget(
        MaterialApp(
          home: SourcesScreen(repository: repository, store: store),
        ),
      );
      await tester.pumpAndSettle();
      final update = find.byKey(const ValueKey('update-hongguo'));
      await tester.tap(update);
      await tester.pump();
      await tester.tap(update);
      expect(repository.operations, ['hongguo:update']);
      expect(tester.widget<FilledButton>(update).onPressed, isNull);
      repository.pending!.complete(
        SourceStatus.fromJson({
          'source': 'hongguo',
          'count': 3,
          'running': true,
          'stage': '查找新剧',
        }),
      );
      await tester.pump();
      await tester.tap(find.text('停止').first);
      await tester.pumpAndSettle();
      expect(repository.operations, ['hongguo:update', 'hongguo:cancel']);
      expect(find.text('已停止'), findsOneWidget);
      expect(find.text('3 部'), findsWidgets);
      await tester.pumpWidget(const SizedBox.shrink());
      store.dispose();
    },
  );

  testWidgets(
    'source manager requests only the current user permitted compiled sources',
    (tester) async {
      SharedPreferences.setMockInitialValues({
        'profiles': jsonEncode([
          const LocalProfile(id: 'default', name: '管理员', admin: true).toJson(),
          const LocalProfile(
            id: 'viewer',
            name: '只看红果',
            sources: ['hongguo'],
            download: false,
          ).toJson(),
        ]),
        'activeProfile': 'viewer',
      });
      final store = testStore(await SharedPreferences.getInstance());
      final repository = SourceFixtureRepository();
      await tester.pumpWidget(
        MaterialApp(
          home: SourcesScreen(repository: repository, store: store),
        ),
      );
      await tester.pumpAndSettle();
      expect(repository.statusRequests, ['hongguo']);
      expect(find.byKey(const ValueKey('source-hongguo')), findsOneWidget);
      for (final source in SourceSite.knownValues.skip(1)) {
        expect(find.byKey(ValueKey('source-${source.id}')), findsNothing);
      }
      await tester.pumpWidget(const SizedBox.shrink());
      store.dispose();
    },
  );

  for (final operation in ['check', 'checkCatalog']) {
    testWidgets(
      'source diagnostics remain collapsed through $operation polling and completion',
      (tester) async {
        SharedPreferences.setMockInitialValues({});
        final store = testStore(await SharedPreferences.getInstance());
        SourceStatus status(String state, {bool running = false}) =>
            SourceStatus.fromJson({
              'source': 'hongguo',
              'count': 7,
              'operation': operation,
              'running': running,
              'stage': running ? '检测连接' : '检测完成',
              'health': {
                'state': state,
                'checkedAt': '2026-09-21T00:00:00Z',
                'sample': '合成检测剧集',
                'steps': [
                  {
                    'name': '入口与目录',
                    'state': 'ok',
                    'message': '目录可达',
                    'httpStatus': 200,
                  },
                ],
              },
            });
        final repository = SourceFixtureRepository()
          ..statuses['hongguo'] = status('ok')
          ..pending = Completer<SourceStatus>();
        await tester.pumpWidget(
          MaterialApp(
            home: SourcesScreen(repository: repository, store: store),
          ),
        );
        await tester.pumpAndSettle();
        final toggle = find.byKey(const ValueKey('health-toggle-hongguo'));
        final sample = find.text('检测剧集：合成检测剧集');
        expect(toggle, findsOneWidget);
        expect(sample, findsNothing);
        expect(find.byTooltip('复制诊断信息'), findsOneWidget);

        if (operation == 'check') {
          await tester.tap(find.byKey(const ValueKey('check-hongguo')));
        } else {
          await tester.tap(find.byTooltip('红果更多操作'));
          await tester.pumpAndSettle();
          await tester.tap(find.text('仅检测目录'));
        }
        await tester.pump();
        expect(repository.operations, ['hongguo:$operation']);
        expect(sample, findsOneWidget);
        await tester.ensureVisible(toggle);
        await tester.tap(toggle);
        await tester.pump();
        expect(sample, findsNothing);

        final checking = status('checking', running: true);
        repository.statuses['hongguo'] = checking;
        repository.pending!.complete(checking);
        repository.pending = null;
        await tester.pump();
        expect(find.text('检测中'), findsOneWidget);
        expect(sample, findsNothing);

        final requests = repository.statusRequests.length;
        await tester.pump(const Duration(seconds: 2));
        await tester.pump();
        expect(repository.statusRequests.length, greaterThan(requests));
        expect(sample, findsNothing);

        repository.statuses['hongguo'] = status(
          operation == 'check' ? 'ok' : 'catalogOnly',
        );
        await tester.pump(const Duration(seconds: 2));
        await tester.pumpAndSettle();
        expect(
          find.text(operation == 'check' ? '连接检测通过' : '目录正常 · 播放未检测'),
          findsOneWidget,
        );
        expect(sample, findsNothing);

        await tester.tap(toggle);
        await tester.pumpAndSettle();
        expect(sample, findsOneWidget);
        expect(find.text('入口与目录：目录可达'), findsOneWidget);
        await tester.tap(toggle);
        await tester.pumpAndSettle();
        expect(sample, findsNothing);
        await tester.pump(const Duration(seconds: 10));
        await tester.pump();
        expect(sample, findsNothing);
        expect(find.byTooltip('复制诊断信息'), findsOneWidget);
        expect(tester.takeException(), isNull);
        await tester.pumpWidget(const SizedBox.shrink());
        store.dispose();
      },
    );
  }

  testWidgets(
    'source diagnostics distinguish CF playback failure on a narrow screen',
    (tester) async {
      tester.view.physicalSize = const Size(320, 900);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);
      SharedPreferences.setMockInitialValues({});
      final store = testStore(await SharedPreferences.getInstance());
      final repository = SourceFixtureRepository();
      repository.statuses['hongguo'] = SourceStatus.fromJson({
        'source': 'hongguo',
        'count': 7,
        'page': 3,
        'hasMore': true,
        'error': '合成 Cloudflare 验证错误',
        'health': {
          'state': 'failed',
          'checkedAt': '2026-09-19T03:00:00Z',
          'sample': '无图合成数据',
          'steps': [
            {
              'name': '入口与目录',
              'state': 'ok',
              'message': '目录可达',
              'httpStatus': 200,
            },
            {
              'name': '播放地址与播放列表',
              'state': 'failed',
              'message': 'Cloudflare 要求浏览器验证',
              'httpStatus': 403,
              'host': 'example.test',
            },
          ],
        },
      });
      await tester.pumpWidget(
        MaterialApp(
          builder: (context, child) => MediaQuery(
            data: MediaQuery.of(
              context,
            ).copyWith(textScaler: const TextScaler.linear(1.5)),
            child: child!,
          ),
          home: SourcesScreen(repository: repository, store: store),
        ),
      );
      await tester.pumpAndSettle();
      expect(find.textContaining('检测未通过'), findsOneWidget);
      expect(find.textContaining('HTTP 403'), findsNothing);
      final toggle = find.byKey(const ValueKey('health-toggle-hongguo'));
      await tester.ensureVisible(toggle);
      // LibreTV 导入卡片使页面内容高于窄屏视口，ensureVisible 的滚动
      // 动画需要一次 settle 后再 tap，否则落点还在滚动前的位置。
      await tester.pumpAndSettle();
      await tester.tap(toggle);
      await tester.pumpAndSettle();
      expect(find.textContaining('HTTP 403'), findsOneWidget);
      expect(find.textContaining('连接检测通过'), findsNothing);
      await tester.tap(toggle);
      await tester.pumpAndSettle();
      expect(find.textContaining('HTTP 403'), findsNothing);
      expect(find.textContaining('检测未通过'), findsOneWidget);
      expect(tester.takeException(), isNull);
      await tester.pumpWidget(const SizedBox.shrink());
      store.dispose();
    },
  );

  testWidgets('libretv import adds runtime source and removal clears it', (
    tester,
  ) async {
    // 视口要足够高：ListView 会裁剪视口外的子元素，卡片与片源列表需同屏
    // 才能同时断言「名称各出现一次」。
    tester.view.physicalSize = const Size(390, 2200);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    addTearDown(() => SourceSite.setRuntimeSources(const []));
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    final repository = SourceFixtureRepository()
      ..libretvSitesResult = [
        const LibreTVSite(
          key: 'abc123',
          name: '测试采集站',
          api: 'https://demo.test/provide/vod',
          count: 12,
        ),
      ];
    await tester.pumpWidget(
      MaterialApp(
        home: SourcesScreen(repository: repository, store: store),
      ),
    );
    await tester.pumpAndSettle();
    // 导入卡片头部来自原生核心的权威列表；已导入站点同时以运行时源
    // 身份出现在下方源卡片（导入即授权，默认可见）。
    expect(find.text('LibreTV / 扩展片源'), findsOneWidget);
    expect(find.text('已导入 1 个来源（1 片源 · 0 脚本）'), findsOneWidget);
    expect(find.text('测试采集站'), findsOneWidget);
    expect(SourceSite.isKnown('libretv:abc123'), isTrue);
    expect(SourceSite.byId('libretv:abc123').isRuntime, isTrue);

    // 展开卡片：显示采集地址、缓存剧集数与导入入口。
    await tester.tap(find.text('LibreTV / 扩展片源'));
    await tester.pumpAndSettle();
    expect(
      find.textContaining('https://demo.test/provide/vod'),
      findsOneWidget,
    );
    expect(find.textContaining('12 部'), findsOneWidget);

    // 导入：粘贴裸 provide/vod 地址 → 对话框 → 导入。
    final importButton = find.text('导入片源 / 脚本');
    await tester.ensureVisible(importButton);
    await tester.tap(importButton);
    await tester.pumpAndSettle();
    expect(find.text('导入 LibreTV 片源 / drpy 脚本'), findsOneWidget);
    await tester.enterText(
      find.byType(TextField),
      'https://new.test/provide/vod',
    );
    await tester.tap(find.text('导入'));
    await tester.pumpAndSettle();
    expect(repository.libretvImports, ['https://new.test/provide/vod']);
    expect(find.textContaining('新增 1 个片源 · 已验证 1 个'), findsOneWidget);
    // 卡片列表与源卡片各出现一次名称。
    expect(find.text('新采集站'), findsNWidgets(2));
    expect(find.text('已导入 2 个来源（2 片源 · 0 脚本）'), findsOneWidget);
    expect(SourceSite.isKnown('libretv:new456'), isTrue);

    // 删除新导入的站点（确认对话框 → 删除）；卡片保持展开。
    final deleteNew = find.byTooltip('删除新采集站');
    await tester.ensureVisible(deleteNew);
    await tester.tap(deleteNew);
    await tester.pumpAndSettle();
    expect(find.textContaining('删除「新采集站」'), findsOneWidget);
    await tester.tap(find.text('删除'));
    await tester.pumpAndSettle();
    expect(repository.libretvRemovals, ['new456']);
    expect(find.text('新采集站'), findsNothing);
    expect(SourceSite.isKnown('libretv:new456'), isFalse);
    // 原有站点不受影响（卡片列表 + 源卡片）。
    expect(find.text('测试采集站'), findsNWidgets(2));
    expect(SourceSite.isKnown('libretv:abc123'), isTrue);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });

  testWidgets('libretv import failure surfaces the server message', (
    tester,
  ) async {
    tester.view.physicalSize = const Size(390, 1400);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    addTearDown(() => SourceSite.setRuntimeSources(const []));
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    final repository = SourceFixtureRepository()..libretvFail = true;
    await tester.pumpWidget(
      MaterialApp(
        home: SourcesScreen(repository: repository, store: store),
      ),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.text('LibreTV / 扩展片源'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('导入片源 / 脚本'));
    await tester.pumpAndSettle();
    await tester.enterText(find.byType(TextField), '坏输入');
    await tester.tap(find.text('导入'));
    await tester.pumpAndSettle();
    expect(
      find.textContaining('导入失败：配置解析失败，请确认是 LibreTV/TVBox 格式的 JSON'),
      findsOneWidget,
    );
    // 失败不注册任何运行时源。
    expect(SourceSite.runtimeValues, isEmpty);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });

  testWidgets('libretv import surfaces drpy candidates for confirmation', (
    tester,
  ) async {
    tester.view.physicalSize = const Size(390, 1400);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    addTearDown(() => SourceSite.setRuntimeSources(const []));
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    final repository = SourceFixtureRepository()
      ..libretvDrpyCandidates = const [
        DrpyCandidate(name: '脚本一', api: 'https://a.example.com/drpy.js'),
        DrpyCandidate(name: '脚本二', api: 'https://b.example.com/drpy2.js'),
      ];
    await tester.pumpWidget(
      MaterialApp(
        home: SourcesScreen(repository: repository, store: store),
      ),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.text('LibreTV / 扩展片源'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('导入片源 / 脚本'));
    await tester.pumpAndSettle();
    // JSON 配置（非 drpy 输入）走 LibreTV 导入链路。
    await tester.enterText(
      find.byType(TextField),
      '{"sites":[{"key":"a","name":"A","type":1,'
      '"api":"https://a.example.com/provide/vod"}]}',
    );
    await tester.tap(find.text('导入'));
    await tester.pumpAndSettle();

    // 混合订阅里的 drpy 脚本候选弹确认框，不自动导入。
    expect(find.text('发现 drpy 脚本候选'), findsOneWidget);
    expect(find.text('脚本一'), findsOneWidget);
    expect(find.text('脚本二'), findsOneWidget);
    expect(repository.drpyImports, isEmpty);

    // 全部导入：两个候选依次走既有 drpy 导入链路。
    await tester.tap(find.text('全部导入'));
    await tester.pumpAndSettle();
    expect(repository.drpyImports, [
      'https://a.example.com/drpy.js',
      'https://b.example.com/drpy2.js',
    ]);
    expect(find.text('已导入'), findsNWidgets(2));
    expect(find.text('完成'), findsOneWidget);

    // 关闭确认框后脚本源出现在列表与运行时注册表
    // （导入反馈 snackbar 已在 pumpAndSettle 中走完生命周期自动消失）。
    await tester.tap(find.text('完成'));
    await tester.pumpAndSettle();
    expect(find.text('发现 drpy 脚本候选'), findsNothing);
    expect(find.text('演示脚本源'), findsNWidgets(2));
    expect(SourceSite.isKnown('drpy:rule123'), isTrue);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });

  testWidgets('libretv parse selector switches the playback parser', (
    tester,
  ) async {
    tester.view.physicalSize = const Size(390, 1600);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    addTearDown(() => SourceSite.setRuntimeSources(const []));
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    final repository = SourceFixtureRepository()
      ..parseConfigResult = const LibreTVParseConfig(
        parses: [
          LibreTVParse(
            name: '本地脚本',
            type: 2,
            url: 'https://a.example.com/p.js',
            kind: '本地脚本',
            script: true,
          ),
          LibreTVParse(
            name: 'JSON 接口',
            type: 1,
            url: 'https://a.example.com/api',
            kind: 'JSON 接口',
          ),
        ],
      );
    await tester.pumpWidget(
      MaterialApp(
        home: SourcesScreen(repository: repository, store: store),
      ),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.text('LibreTV / 扩展片源'));
    await tester.pumpAndSettle();

    // 订阅带 parses 时展示「播放解析方式」下拉，默认直链/嗅探。
    expect(find.text('播放解析方式'), findsOneWidget);
    expect(find.textContaining('直链 / 嗅探'), findsWidgets);

    await tester.tap(find.byKey(const ValueKey('libretv-parse-select')));
    await tester.pumpAndSettle();
    await tester.tap(find.text('本地脚本 · 本地脚本').last);
    await tester.pumpAndSettle();

    expect(repository.parseSelections, ['https://a.example.com/p.js']);
    expect(find.textContaining('解析方式已切换为「本地脚本」'), findsOneWidget);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });

  testWidgets('catvod spiders are labelled in the drpy list', (tester) async {
    tester.view.physicalSize = const Size(390, 1600);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    addTearDown(() => SourceSite.setRuntimeSources(const []));
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    final repository = SourceFixtureRepository()
      ..drpySitesResult = const [
        DrpySite(
          key: 'cat123',
          name: '兔小贝[cat]',
          scriptUrl: 'https://a.example.com/cat/tuxiaobei_open.js',
          engine: 'catvod',
        ),
        DrpySite(key: 'rule456', name: '普通脚本源'),
      ];
    await tester.pumpWidget(
      MaterialApp(
        home: SourcesScreen(repository: repository, store: store),
      ),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.text('LibreTV / 扩展片源'));
    await tester.pumpAndSettle();

    // CatVod 方言加标记，普通 drpy2 脚本不加。
    expect(find.text('CatVod'), findsOneWidget);
    // 站点名同时出现在上方站源卡片与下方脚本列表，各一次。
    expect(find.text('兔小贝[cat]'), findsWidgets);
    expect(find.text('普通脚本源'), findsWidgets);
    expect(repository.drpySitesResult.first.isCatVod, isTrue);
    expect(repository.drpySitesResult.last.isCatVod, isFalse);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });
}
