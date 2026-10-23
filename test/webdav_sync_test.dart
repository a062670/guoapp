import 'dart:convert';

import 'package:duanju_app/follow_state.dart';
import 'package:duanju_app/lan_sync_models.dart';
import 'package:duanju_app/local_store.dart';
import 'package:duanju_app/models.dart';
import 'package:duanju_app/webdav_sync.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';

class FakeWebDavTransport implements WebDavTransport {
  FakeWebDavTransport({Map<String, String>? files, this.failWrite = false})
    : files = files ?? <String, String>{};
  final Map<String, String> files;
  final bool failWrite;
  final List<String> reads = [];

  /// 写入时请求的完整地址，用于校验文件确实落在同步目录里。
  final List<String> writes = [];

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
    final name = file.pathSegments.last;
    reads.add(name);
    final content = files[name];
    if (content == null) throw StateError('远端没有此文件');
    return content;
  }

  @override
  Future<void> write(Uri file, String content) async {
    if (failWrite) throw StateError('模拟上传失败');
    writes.add(file.toString());
    files[file.pathSegments.last] = content;
  }
}

Future<LocalStore> createStore() async {
  SharedPreferences.setMockInitialValues({});
  final prefs = await SharedPreferences.getInstance();
  final store = LocalStore(prefs);
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

Future<WebDavSyncOutcome> run(
  LocalStore store,
  WebDavTransport transport,
) => WebDavSynchronizer(store: store, transport: transport).synchronize();

WatchEntry watch(Drama drama, int episode, double position) => WatchEntry(
  drama: drama,
  episode: episode,
  position: position,
  duration: 100,
  updatedAt: DateTime.utc(2026, 10, 14, 12),
);

void main() {
  const dramaA = Drama(id: 'hongguo:1', source: 'hongguo', title: '甲剧');
  const dramaB = Drama(id: 'hongguo:2', source: 'hongguo', title: '乙剧');

  test('webdav settings round trip and validation', () async {
    final store = await createStore();
    expect(store.webDavSettings.enabled, isFalse);
    expect(store.webDavSettings.directory, WebDavSettings.defaultDirectory);
    await configure(store);
    final saved = store.webDavSettings;
    expect(saved.enabled, isTrue);
    expect(saved.url, 'https://dav.example.com');
    expect(
      saved.directoryUri.toString(),
      'https://dav.example.com/zhenguoju-sync/',
    );
    expect(
      WebDavSettings(
        url: 'https://dav.example.com/dav/',
      ).directoryUri.toString(),
      'https://dav.example.com/dav/zhenguoju-sync/',
    );
    expect(saved.directoryUri!.path.endsWith('/'), isTrue);
    expect(WebDavSettings(url: 'ftp://dav.example.com').directoryUri, isNull);
    expect(
      WebDavSettings(url: 'https://dav.example.com', directory: 'a b').directoryUri,
      isNull,
    );
    expect(
      WebDavSettings.fromJson('broken').enabled,
      isFalse,
    );
    expect(
      WebDavSettings.fromJson({'enabled': true}).configured,
      isFalse,
    );
  });

  test('webdav password stays out of exported backup', () async {
    final store = await createStore();
    await configure(store);
    final backup = await store.exportBackup();
    expect(backup.contains('dav.example.com'), isFalse);
    expect(backup.contains('webdav'), isFalse);
  });

  test('unconfigured synchronizer reports missing settings', () async {
    final store = await createStore();
    await expectLater(
      run(store, FakeWebDavTransport()),
      throwsStateError,
    );
  });

  test('two devices merge follows, progress and unfollows', () async {
    final a = await createStore();
    await configure(a);
    final transport = FakeWebDavTransport();
    await run(a, transport);
    await a.toggleFavorite(dramaA);
    await a.saveWatch(watch(dramaA, 2, 30));
    await run(a, transport);

    final b = await createStore();
    await configure(b);
    final outcome = await run(b, transport);
    expect(outcome.added, 1);
    expect(outcome.conflicts, 0);
    expect(b.isFavorite(dramaA.id), isTrue);
    expect(b.watched(dramaA.id)?.episode, 2);

    await b.toggleFavorite(dramaB);
    await run(b, transport);
    await run(a, transport);
    expect(a.isFavorite(dramaB.id), isTrue);

    await a.toggleFavorite(dramaA);
    await run(a, transport);
    await run(b, transport);
    expect(b.isFavorite(dramaA.id), isFalse);
    expect(b.watched(dramaA.id)?.episode, 2);
  });

  test('concurrent edits keep both versions as a conflict and resolve', () async {
    final a = await createStore();
    await configure(a);
    final transport = FakeWebDavTransport();
    await a.toggleFavorite(dramaA);
    await run(a, transport);

    final b = await createStore();
    await configure(b);
    await run(b, transport);
    expect(b.isFavorite(dramaA.id), isTrue);

    await a.setFollowStatus(dramaA, FollowStatus.watched);
    await b.setFollowStatus(dramaA, FollowStatus.watching);
    await run(b, transport);
    final outcome = await run(a, transport);
    expect(outcome.conflicts, 1);
    final record = a.lanDocument.records[dramaA.id]!;
    expect(record.conflicts, 1);
    expect(record.fields['status']!.conflict, isTrue);

    final watched = record.fields['status']!.values.firstWhere(
      (candidate) =>
          (candidate.value as Map<String, dynamic>)['status'] == 'watched',
    );
    await a.resolveLanConflict(dramaA.id, 'status', watched, record.hash);
    expect(a.lanDocument.records[dramaA.id]!.conflicts, 0);

    await run(a, transport);
    await run(b, transport);
    expect(b.lanDocument.records[dramaA.id]!.conflicts, 0);
  });

  test('own and corrupt remote files are skipped', () async {
    final a = await createStore();
    await configure(a);
    final transport = FakeWebDavTransport();
    await a.toggleFavorite(dramaA);
    await run(a, transport);
    final replica = a.lanDocument.replica;
    expect(transport.files.containsKey('$replica.json'), isTrue);

    transport.files['ffffffffffffffffffffffffffffffff.json'] = 'not json';
    final outcome = await run(a, transport);
    expect(outcome.hasChanges, isFalse);
    expect(transport.reads, isNot(contains('$replica.json')));
    expect(transport.reads, contains('ffffffffffffffffffffffffffffffff.json'));
    expect(a.isFavorite(dramaA.id), isTrue);
  });

  test('upload lands inside the sync directory, not its parent', () async {
    final a = await createStore();
    // 坚果云形态：地址本身带 WebDAV 目录前缀，容易少一级路径
    await a.saveWebDavSettings(
      const WebDavSettings(
        enabled: true,
        url: 'https://dav.jianguoyun.com/dav/',
        username: 'user',
        password: 'pass',
      ),
    );
    final transport = FakeWebDavTransport();
    await a.toggleFavorite(dramaA);
    await run(a, transport);
    final replica = a.lanDocument.replica;
    expect(
      transport.writes.single,
      'https://dav.jianguoyun.com/dav/zhenguoju-sync/$replica.json',
    );
  });

  test('network failure keeps local records untouched', () async {
    final a = await createStore();
    await configure(a);
    final broken = _FailingTransport();
    await a.toggleFavorite(dramaA);
    await a.saveWatch(watch(dramaA, 2, 30));
    await expectLater(run(a, broken), throwsStateError);
    expect(a.isFavorite(dramaA.id), isTrue);
    expect(a.watched(dramaA.id)?.episode, 2);
  });

  test('merge is applied locally even when the upload fails', () async {
    final a = await createStore();
    await configure(a);
    final shared = <String, String>{};
    await a.toggleFavorite(dramaA);
    await a.saveWatch(watch(dramaA, 2, 30));
    await run(a, FakeWebDavTransport(files: shared));

    final b = await createStore();
    await configure(b);
    await expectLater(
      run(b, FakeWebDavTransport(files: shared, failWrite: true)),
      throwsStateError,
    );
    expect(b.isFavorite(dramaA.id), isTrue);
    expect(b.watched(dramaA.id)?.episode, 2);
  });

  test('records outside the local user sources are filtered', () async {
    final a = await createStore();
    await a.saveProfile(
      id: 'default',
      name: '管理员',
      sources: [],
      download: true,
      pin: 'abcdef12',
    );
    await configure(a);
    const foreign = Drama(id: 'hanxiaoquan:9', source: 'hanxiaoquan', title: '外站剧');
    await a.toggleFavorite(dramaA);
    await a.toggleFavorite(foreign);
    final transport = FakeWebDavTransport();
    await run(a, transport);

    final b = await createStore();
    await configure(b);
    await b.saveProfile(
      id: 'default',
      name: '管理员',
      sources: [],
      download: true,
      pin: 'abcdef12',
    );
    await b.saveProfile(name: '访客', sources: ['hongguo'], download: false);
    final visitor = b.profiles.firstWhere((profile) => !profile.admin);
    await b.switchProfile(visitor.id);
    await run(b, transport);
    expect(b.isFavorite(dramaA.id), isTrue);
    expect(b.isFavorite(foreign.id), isFalse);
  });

  test('purge remote source subs replaces cloud list with local', () async {
    final store = await createStore();
    await configure(store);
    final self = store.lanDocument.replica;
    // 云端两台设备的文件：本机 + 另一台（带着已失效的旧站源）。
    final files = <String, String>{
      '$self.json': jsonEncode({
        'schema': WebDavSynchronizer.schema,
        'document': {
          'replica': self,
          'counter': 1,
          'records': {},
          'sourceSubs': {
            'libretv:old': {
              'name': '本机旧源',
              'api': 'https://old.example.com',
              'at': 0,
            },
          },
        },
      }),
      'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.json': jsonEncode({
        'schema': WebDavSynchronizer.schema,
        'document': {
          'replica': 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
          'counter': 1,
          'records': {},
          'sourceSubs': {
            'libretv:dead1': {
              'name': '失效源一',
              'api': 'https://d1.example.com',
              'at': 5,
            },
            'libretv:dead2': {
              'name': '失效源二',
              'api': 'https://d2.example.com',
              'at': 6,
            },
          },
        },
      }),
    };
    final transport = FakeWebDavTransport(files: files);
    final result = await WebDavSynchronizer(
      store: store,
      transport: transport,
    ).purgeRemoteSourceSubs();

    // 本机没配片源仓库 → 本机清单为空，两处云端清单都被清空。
    expect(result.files, 2);
    expect(result.cleared, 2);
    expect(result.local, 0);

    final other = jsonDecode(files['bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.json']!) as Map;
    final otherDoc = other['document'] as Map;
    expect((otherDoc['sourceSubs'] as Map).isEmpty, isTrue);
    // 追剧记录不能被牵连。
    expect(otherDoc.containsKey('records'), isTrue);
  });

  test('purge remote source subs overwrites own replica too', () async {
    final store = await createStore();
    await configure(store);
    final self = store.lanDocument.replica;
    final files = <String, String>{
      '$self.json': jsonEncode({
        'schema': WebDavSynchronizer.schema,
        'document': {
          'replica': self,
          'counter': 1,
          'sourceSubs': {
            'libretv:gone': {
              'name': '已删除源',
              'api': 'https://gone.example.com',
              'at': 0,
            },
          },
        },
      }),
      'cccccccccccccccccccccccccccccccc.json': jsonEncode({
        'schema': WebDavSynchronizer.schema,
        'document': {
          'replica': 'cccccccccccccccccccccccccccccccc',
          'sourceSubs': {
            'libretv:dead': {
              'name': '失效源',
              'api': 'https://d.example.com',
              'at': 1,
            },
          },
        },
      }),
    };
    final transport = FakeWebDavTransport(files: files);
    final result = await WebDavSynchronizer(
      store: store,
      transport: transport,
    ).purgeRemoteSourceSubs();

    expect(result.files, 2);
    expect(result.cleared, 1, reason: '只统计其他设备被清掉的条目');
    expect(result.local, 0);

    // 本机文件同样被「本机当前清单」覆盖：本机没有站源时旧条目必须清掉。
    final mine = jsonDecode(files['$self.json']!) as Map;
    final subs = (mine['document'] as Map)['sourceSubs'] as Map;
    expect(subs.containsKey('libretv:gone'), isFalse);
    expect(subs.isEmpty, isTrue);
  });

  test('purge remote source subs skips non-sync files', () async {
    final store = await createStore();
    await configure(store);
    // 目录里只有非同步文件：不能被改写，且要给出明确提示。
    final files = <String, String>{
      'readme.txt': 'not a sync file',
      'broken.json': 'not json at all',
    };
    final transport = FakeWebDavTransport(files: files);
    expect(
      () => WebDavSynchronizer(
        store: store,
        transport: transport,
      ).purgeRemoteSourceSubs(),
      throwsA(isA<StateError>()),
    );
    expect(transport.writes, isEmpty, reason: '非同步文件不应被改写');
  });
}

class _FailingTransport implements WebDavTransport {
  @override
  Future<void> ensureDirectory(Uri directory) async =>
      throw StateError('模拟网络故障');
  @override
  Future<List<WebDavRemoteFile>> list(Uri directory) async =>
      throw StateError('模拟网络故障');
  @override
  Future<String> read(Uri file) async => throw StateError('模拟网络故障');
  @override
  Future<void> write(Uri file, String content) async =>
      throw StateError('模拟网络故障');
}
