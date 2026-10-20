import 'dart:convert';

import 'package:duanju_app/core_bridge.dart';
import 'package:duanju_app/drpy.dart';
import 'package:duanju_app/lan_sync_models.dart';
import 'package:duanju_app/libretv.dart';
import 'package:duanju_app/local_store.dart';
import 'package:duanju_app/models.dart';
import 'package:duanju_app/webdav_sync.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:shared_preferences_platform_interface/shared_preferences_platform_interface.dart';

// WebDAV 片源同步（+124）：
// - 同步范围扩展到「片源订阅 + 收藏 / 隐藏状态」；
// - 偏好合并规则：per-source 最新时间戳优先，取消操作可传播
//   （靠取消墓碑实现，不是只增不减的并集）；
// - 对端设备导入、本机没有的 LibreTV / drpy 源在同步时自动导入，
//   幂等；粘贴导入的 drpy 脚本源无地址可同步，跳过并警告。

class FakeWebDavTransport implements WebDavTransport {
  FakeWebDavTransport({Map<String, String>? files})
    : files = files ?? <String, String>{};
  final Map<String, String> files;

  @override
  Future<void> ensureDirectory(Uri directory) async {}

  @override
  Future<List<WebDavRemoteFile>> list(Uri directory) async => [
    for (final entry in files.entries)
      WebDavRemoteFile(
        name: entry.key,
        uri: directory.resolve(entry.key),
        bytes: utf8.encode(entry.value).length,
      ),
  ];

  @override
  Future<String> read(Uri file) async {
    final content = files[file.pathSegments.last];
    if (content == null) throw StateError('远端没有此文件');
    return content;
  }

  @override
  Future<void> write(Uri file, String content) async {
    files[file.pathSegments.last] = content;
  }
}

/// 模拟原生核心的片源仓库：记录导入调用，导入后把站点登记进列表
/// （对应 Go 侧 libretv.json / drpy.json 落库），使同步器刷新运行时
/// 注册表后 `SourceSite.isKnown` 能识别新源。
class FakeSourceRepository extends AppRepository {
  FakeSourceRepository({
    List<LibreTVSite>? sites,
    List<DrpySite>? scripts,
  }) : _sites = sites ?? <LibreTVSite>[],
       _scripts = scripts ?? <DrpySite>[];

  List<LibreTVSite> _sites;
  List<DrpySite> _scripts;
  final List<String> libretvImports = [];
  final List<String> drpyImports = [];

  /// 会触发导入失败的地址（模拟失效订阅 / 脚本下载 404）。
  final Set<String> failures = {};

  @override
  Future<List<LibreTVSite>> libretvSites() async => _sites;

  @override
  Future<List<DrpySite>> drpySites() async => _scripts;

  @override
  Future<LibreTVImportResult> libretvImport(String subscription) async {
    libretvImports.add(subscription);
    if (failures.contains(subscription)) throw StateError('模拟导入失败');
    final key = siteKeyOf(subscription);
    _sites = [
      ..._sites,
      LibreTVSite(key: key, name: '导入站 $key', api: subscription),
    ];
    return const LibreTVImportResult(
      added: [],
      skipped: [],
      drpy: [],
      verified: 0,
    );
  }

  @override
  Future<DrpyImportResult> drpyImport(String script) async {
    drpyImports.add(script);
    if (failures.contains(script)) throw StateError('模拟导入失败');
    final key = siteKeyOf(script);
    _scripts = [
      ..._scripts,
      DrpySite(key: key, name: '导入脚本 $key', scriptUrl: script),
    ];
    return DrpyImportResult(key: key, name: '导入脚本 $key');
  }

  // ---- 以下为 AppRepository 其余抽象方法的最小实现（本测试用不到） ----

  @override
  Future<void> initialize() async {}

  @override
  Future<CatalogPage> catalog(
    String source, {
    int page = 1,
    String query = '',
    String category = '',
    bool force = false,
  }) async => CatalogPage([]);

  @override
  Future<CatalogPage> cached(String source, {String category = ''}) async =>
      CatalogPage([]);

  @override
  Future<String> cover(Drama drama, {bool force = false}) async => '';

  @override
  Future<DramaDetail> detail(Drama drama) async =>
      throw AppFailure('合成仓库没有详情');

  @override
  Future<PlaybackPlan> resolve(
    Drama drama,
    Episode episode, {
    int quality = 0,
    int route = 0,
  }) async => const PlaybackPlan(url: 'https://example.test/synthetic.mp4');

  @override
  Future<PlaybackPlan> fallback(PlaybackPlan current) async =>
      throw AppFailure('没有备用线路');

  @override
  Future<void> cancelPlayback() async {}

  @override
  Future<void> release(String session) async {}

  @override
  Future<List<String>> playbackProbe() async => const [];
}

/// 测试约定的站点 key 派生规则：取 URL 主机名第一段。
/// 远端订阅条目与 FakeSourceRepository 导入登记共用，保证源 ID 一致。
String siteKeyOf(String url) => Uri.parse(url).host.split('.').first;

/// 伪造一台「对端设备」已上传的同步文件（只带片源偏好 / 订阅，
/// 无追剧记录——记录合并已有 webdav_sync_test 覆盖）。
Map<String, String> remoteFiles({
  required String replica,
  Map<String, LanSourcePref> prefs = const {},
  Map<String, LanSourceSub> subs = const {},
}) {
  final document = LanDocument(
    replica: replica,
    sourcePrefs: prefs,
    sourceSubs: subs,
  );
  return {
    '$replica.json': jsonEncode({
      'schema': 1,
      'savedAt': DateTime.utc(2026, 10, 20, 12).toIso8601String(),
      'document': document.toJson(),
    }),
  };
}

Future<LocalStore> createStore([
  InMemorySharedPreferencesStore? platform,
]) async {
  SharedPreferences.resetStatic();
  if (platform == null) {
    SharedPreferences.setMockInitialValues({});
  } else {
    SharedPreferencesStorePlatform.instance = platform;
  }
  final store = LocalStore(await SharedPreferences.getInstance());
  addTearDown(store.dispose);
  return store;
}

Future<void> configure(LocalStore store) => store.saveWebDavSettings(
  const WebDavSettings(
    enabled: true,
    url: 'https://dav.example.com',
    username: 'user',
    password: 'pass',
  ),
);

Future<WebDavSyncOutcome> sync(
  LocalStore store,
  WebDavTransport transport,
  AppRepository? repository,
) => WebDavSynchronizer(
  store: store,
  transport: transport,
  repository: repository,
).synchronize();

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();

  // SourceSite 运行时注册表是进程内全局状态：每个用例前后清空，
  // 避免「对端导入的源」泄漏到其他用例的 isKnown 判断里。
  setUp(() => SourceSite.setRuntimeSources(const []));
  tearDown(() => SourceSite.setRuntimeSources(const []));

  test('收藏与隐藏状态经 WebDAV 双向同步，取消操作可传播', () async {
    final a = await createStore();
    await configure(a);
    final transport = FakeWebDavTransport();
    await a.toggleFavoriteSource('libretv:aaa');
    await sync(a, transport, null);

    // 设备 B 首次同步即拿到 A 的收藏状态。
    final b = await createStore();
    await configure(b);
    await sync(b, transport, null);
    expect(b.isFavoriteSource('libretv:aaa'), isTrue);

    // B 追加隐藏，A 同步后生效。
    await b.toggleSourceHidden('libretv:aaa');
    await sync(b, transport, null);
    await sync(a, transport, null);
    expect(a.isHiddenSource('libretv:aaa'), isTrue);

    // A 取消收藏：取消墓碑随同步传播到 B（非只增不减并集）。
    await a.toggleFavoriteSource('libretv:aaa');
    await sync(a, transport, null);
    await sync(b, transport, null);
    expect(b.isFavoriteSource('libretv:aaa'), isFalse);
    expect(b.isHiddenSource('libretv:aaa'), isTrue);

    // 再次收敛同步不应产生新的偏好变化。
    final settled = await sync(b, transport, null);
    expect(settled.sourcePrefs, 0);
    expect(settled.hasChanges, isFalse);
  });

  test('旧版本地格式（无时间戳数组）兼容读取并参与同步', () async {
    SharedPreferences.resetStatic();
    SharedPreferences.setMockInitialValues({
      'favoriteSources': '["libretv:legacy"]',
    });
    final a = LocalStore(await SharedPreferences.getInstance());
    addTearDown(a.dispose);
    expect(a.isFavoriteSource('libretv:legacy'), isTrue);
    // 旧格式没有时间戳，按 0 处理：本机任何重新操作都会更新。
    expect(a.sourcePrefsSnapshot()['libretv:legacy']!.at, 0);

    await configure(a);
    final transport = FakeWebDavTransport();
    await sync(a, transport, null);
    final replica = a.lanDocument.replica;
    final uploaded = jsonDecode(transport.files['$replica.json']!) as Map;
    final document = uploaded['document'] as Map;
    expect(
      (document['sourcePrefs'] as Map)['libretv:legacy'],
      isNotNull,
    );

    final b = await createStore();
    await configure(b);
    await sync(b, transport, null);
    expect(b.isFavoriteSource('libretv:legacy'), isTrue);
  });

  test('对端片源订阅自动导入，且重复同步不重复导入', () async {
    final a = await createStore();
    await configure(a);
    final repository = FakeSourceRepository();
    final api = 'https://x.example.com/provide/vod';
    final transport = FakeWebDavTransport(
      files: remoteFiles(
        replica: 'b' * 32,
        subs: {
          'libretv:${siteKeyOf(api)}': LanSourceSub(
            name: '测试站',
            api: api,
            at: 2,
          ),
        },
      ),
    );

    final outcome = await sync(a, transport, repository);
    expect(repository.libretvImports, [api]);
    expect(SourceSite.isKnown('libretv:${siteKeyOf(api)}'), isTrue);
    expect(outcome.sourceImports, 1);

    // 幂等：本机导入后再次同步不再重复导入。
    final again = await sync(a, transport, repository);
    expect(repository.libretvImports, hasLength(1));
    expect(again.sourceImports, 0);
  });

  test('粘贴导入的 drpy 脚本源无法跨设备同步，跳过并警告', () async {
    final a = await createStore();
    await configure(a);
    final repository = FakeSourceRepository();
    final transport = FakeWebDavTransport(
      files: remoteFiles(
        replica: 'b' * 32,
        subs: const {
          'drpy:pasted': LanSourceSub(name: '粘贴脚本源', api: '', at: 2),
        },
      ),
    );

    final outcome = await sync(a, transport, repository);
    expect(repository.drpyImports, isEmpty);
    expect(outcome.sourceImports, 0);
    expect(
      outcome.warnings.any((warning) => warning.contains('粘贴导入')),
      isTrue,
    );
  });

  test('个别片源导入失败只记警告，不影响其余同步', () async {
    final a = await createStore();
    await configure(a);
    final repository = FakeSourceRepository()
      ..failures.add('https://bad.example.com/provide/vod');
    final transport = FakeWebDavTransport(
      files: remoteFiles(
        replica: 'b' * 32,
        subs: const {
          'libretv:bad': LanSourceSub(
            name: '坏站',
            api: 'https://bad.example.com/provide/vod',
            at: 2,
          ),
        },
      ),
    );

    final outcome = await sync(a, transport, repository);
    expect(outcome.sourceImports, 0);
    expect(
      outcome.warnings.any((warning) => warning.contains('失败')),
      isTrue,
    );
    // 同步本身完成：设置已保存，上传已发生。
    expect(outcome.savedAt, isNotNull);
    expect(transport.files.containsKey('${a.lanDocument.replica}.json'), isTrue);
  });

  test('applySourcePrefs：旧记录不回退、新记录覆盖、并列取并集', () async {
    final store = await createStore();
    await store.toggleFavoriteSource('libretv:aaa');
    final at = store.sourcePrefsSnapshot()['libretv:aaa']!.at;
    expect(store.isFavoriteSource('libretv:aaa'), isTrue);

    // 更旧的对端记录：不生效。
    expect(
      await store.applySourcePrefs({
        'libretv:aaa': const LanSourcePref(
          favorite: false,
          hidden: false,
          at: 0,
        ),
      }),
      0,
    );
    expect(store.isFavoriteSource('libretv:aaa'), isTrue);

    // 时间戳并列：对端同时刻的操作按字段取并集（隐藏生效、收藏保持）。
    expect(
      await store.applySourcePrefs({
        'libretv:aaa': LanSourcePref(
          favorite: true,
          hidden: true,
          at: at,
        ),
      }),
      1,
    );
    expect(store.isFavoriteSource('libretv:aaa'), isTrue);
    expect(store.isHiddenSource('libretv:aaa'), isTrue);

    // 更新的记录：取消传播（两个字段一起回到 false，按条目计 1 项）。
    expect(
      await store.applySourcePrefs({
        'libretv:aaa': LanSourcePref(
          favorite: false,
          hidden: false,
          at: at + 1,
        ),
      }),
      1,
    );
    expect(store.isFavoriteSource('libretv:aaa'), isFalse);
    expect(store.isHiddenSource('libretv:aaa'), isFalse);

    // 非运行时片源 ID 直接被忽略。
    expect(
      await store.applySourcePrefs({
        'hongguo': LanSourcePref(favorite: true, hidden: false, at: at + 2),
      }),
      0,
    );
  });

  test('重启后收藏 / 隐藏状态与取消墓碑保持', () async {
    final platform = InMemorySharedPreferencesStore.withData({});
    final store = await createStore(platform);
    await store.toggleFavoriteSource('libretv:aaa');
    await store.toggleSourceHidden('libretv:aaa');

    final restarted = await createStore(platform);
    expect(restarted.isFavoriteSource('libretv:aaa'), isTrue);
    expect(restarted.isHiddenSource('libretv:aaa'), isTrue);

    // 取消收藏后重启：偏好不回来，隐藏仍在。
    await restarted.toggleFavoriteSource('libretv:aaa');
    final again = await createStore(platform);
    expect(again.isFavoriteSource('libretv:aaa'), isFalse);
    expect(again.isHiddenSource('libretv:aaa'), isTrue);
    expect(again.favoriteSourceIds, isEmpty);
    expect(again.hiddenSourceIds, {'libretv:aaa'});
  });
}
