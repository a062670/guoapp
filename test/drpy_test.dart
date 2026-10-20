import 'dart:async';

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

import 'fixtures.dart';

class DrpyFixtureRepository extends FixtureRepository {
  final libretvSitesResult = <LibreTVSite>[];
  final drpySitesResult = <DrpySite>[];
  final drpyImports = <String>[];
  final drpyRemovals = <String>[];
  final libretvRemovals = <String>[];

  @override
  bool get supportsSourceManagement => true;

  @override
  Future<List<LibreTVSite>> libretvSites() async => libretvSitesResult;

  @override
  Future<List<DrpySite>> drpySites() async => drpySitesResult;

  @override
  Future<void> libretvRemove(String key) async => libretvRemovals.add(key);

  @override
  Future<DrpyImportResult> drpyImport(String script) async {
    drpyImports.add(script);
    final site = DrpySite(key: 'rule123', name: '演示脚本源');
    drpySitesResult.add(site);
    return DrpyImportResult(key: site.key, name: site.name);
  }

  @override
  Future<void> drpyRemove(String key) async {
    drpyRemovals.add(key);
    drpySitesResult.removeWhere((site) => site.key == key);
  }
}

void main() {
  group('drpy site model', () {
    test('from json keeps defaults for a pasted script', () {
      final site = DrpySite.fromJson(const {
        'key': 'rule123',
        'name': '演示脚本源',
      });
      expect(site.key, 'rule123');
      expect(site.name, '演示脚本源');
      expect(site.scriptUrl, '');
      expect(site.title, '');
      expect(site.host, '');
      expect(site.count, 0);
      expect(site.sourceId, 'drpy:rule123');
      expect(site.detailLabel, '脚本源');
    });

    test('from json prefers the rule title and host for the label', () {
      final site = DrpySite.fromJson(const {
        'key': 'rule456',
        'name': '某某影视',
        'title': '团团影院',
        'host': 'https://demo.test',
        'scriptUrl': 'https://demo.test/lib/drpy.js',
        'count': 7,
      });
      expect(site.title, '团团影院');
      expect(site.detailLabel, '团团影院');
      expect(site.scriptUrl, 'https://demo.test/lib/drpy.js');
      expect(site.count, 7);
      final registered = site.toSourceSite();
      expect(registered.id, 'drpy:rule456');
      expect(registered.description, 'drpy 脚本 · 团团影院');
      expect(registered.isRuntime, isTrue);
      expect(registered.groupId, 'drpy');
      expect(registered.groupName, 'drpy 脚本源');
    });

    test('import result parses key and name', () {
      final result = DrpyImportResult.fromJson(const {
        'key': 'rule789',
        'name': '新脚本源',
        'added': true,
        'total': 1,
      });
      expect(result.key, 'rule789');
      expect(result.name, '新脚本源');
    });
  });

  group('drpy runtime registration', () {
    test('runtime registry accepts drpy sources and keeps them visible', () {
      addTearDown(() => SourceSite.setRuntimeSources(const []));
      SourceSite.setRuntimeSources([
        DrpySite(key: 'rule123', name: '演示脚本源').toSourceSite(),
        const LibreTVSite(
          key: 'abc123',
          name: '测试采集站',
          api: 'https://demo.test/provide/vod',
        ).toSourceSite(),
      ]);
      expect(SourceSite.isKnown('drpy:rule123'), isTrue);
      expect(SourceSite.isKnown('libretv:abc123'), isTrue);
      expect(SourceSite.byId('drpy:rule123').groupId, 'drpy');
      // 导入即授权：未解锁时 drpy 源同样默认可见。
      final visible = SourceSite.visibleValues(unlocked: false);
      expect(visible.any((site) => site.id == 'drpy:rule123'), isTrue);
      expect(visible.any((site) => site.id == 'libretv:abc123'), isTrue);
      SourceSite.unregisterRuntime('drpy:rule123');
      expect(SourceSite.isKnown('drpy:rule123'), isFalse);
    });
  });

  testWidgets('runtime card mixes libretv sites and drpy scripts', (
    tester,
  ) async {
    tester.view.physicalSize = const Size(390, 1600);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    addTearDown(() => SourceSite.setRuntimeSources(const []));
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    final repository = DrpyFixtureRepository()
      ..libretvSitesResult.add(
        const LibreTVSite(
          key: 'abc123',
          name: '测试采集站',
          api: 'https://demo.test/provide/vod',
          count: 12,
        ),
      )
      ..drpySitesResult.add(
        const DrpySite(
          key: 'rule123',
          name: '演示脚本源',
          title: '团团影院',
          count: 5,
        ),
      );
    await tester.pumpWidget(
      MaterialApp(
        home: SourcesScreen(repository: repository, store: store),
      ),
    );
    await tester.pumpAndSettle();
    // 卡片副标题汇总两类来源。
    expect(find.text('LibreTV / 扩展片源'), findsOneWidget);
    expect(find.text('已导入 2 个来源（1 片源 · 1 脚本）'), findsOneWidget);
    expect(SourceSite.isKnown('drpy:rule123'), isTrue);

    // 展开卡片：两区条目同卡渲染。
    await tester.tap(find.text('LibreTV / 扩展片源'));
    await tester.pumpAndSettle();
    expect(find.text('LibreTV 片源'), findsOneWidget);
    expect(find.text('drpy 脚本源'), findsOneWidget);
    // 两类条目同时出现在卡片列表与下方运行时源卡片（导入即授权）。
    expect(find.text('测试采集站'), findsNWidgets(2));
    expect(find.text('演示脚本源'), findsNWidgets(2));
    expect(find.textContaining('团团影院'), findsOneWidget);

    // 删除 drpy 条目：走 drpyRemove，卡片与注册表同步清除。
    final deleteScript = find.byTooltip('删除演示脚本源');
    await tester.ensureVisible(deleteScript);
    await tester.tap(deleteScript);
    await tester.pumpAndSettle();
    await tester.tap(find.text('删除'));
    await tester.pumpAndSettle();
    expect(repository.drpyRemovals, ['rule123']);
    expect(repository.libretvRemovals, isEmpty);
    expect(SourceSite.isKnown('drpy:rule123'), isFalse);
    expect(find.text('演示脚本源'), findsNothing);
    expect(find.text('测试采集站'), findsNWidgets(2));
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });

  testWidgets('script input routes to drpy import', (tester) async {
    tester.view.physicalSize = const Size(390, 1600);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);
    addTearDown(() => SourceSite.setRuntimeSources(const []));
    SharedPreferences.setMockInitialValues({});
    final store = testStore(await SharedPreferences.getInstance());
    final repository = DrpyFixtureRepository();
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
    await tester.enterText(
      find.byType(TextField),
      'var rule = {"title":"演示脚本源","host":"https://demo.test"};',
    );
    await tester.tap(find.text('导入'));
    await tester.pumpAndSettle();
    expect(repository.drpyImports.length, 1);
    expect(repository.drpyImports.single, contains('var rule'));
    expect(find.textContaining('已导入脚本源「演示脚本源」'), findsOneWidget);
    expect(SourceSite.isKnown('drpy:rule123'), isTrue);
    await tester.pumpWidget(const SizedBox.shrink());
    store.dispose();
  });
}
