import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';

import 'core_bridge.dart';
import 'drpy.dart';
import 'lan_sync_models.dart';
import 'libretv.dart';
import 'local_store.dart';
import 'models.dart';

class WebDavRemoteFile {
  const WebDavRemoteFile({required this.name, required this.uri, this.bytes = -1});
  final String name;
  final Uri uri;
  final int bytes;
}

abstract class WebDavTransport {
  Future<void> ensureDirectory(Uri directory);
  Future<List<WebDavRemoteFile>> list(Uri directory);
  Future<String> read(Uri file);
  Future<void> write(Uri file, String content);
}

class HttpWebDavTransport implements WebDavTransport {
  HttpWebDavTransport({required this.username, required this.password});

  static const fileLimit = 8 * 1024 * 1024;

  final String username;
  final String password;
  HttpClient? _client;

  HttpClient get _http =>
      _client ??= (HttpClient()
        ..connectionTimeout = const Duration(seconds: 15)
        ..idleTimeout = const Duration(seconds: 60));

  void close() {
    _client?.close(force: true);
    _client = null;
  }

  Future<HttpClientResponse> _send(
    String method,
    Uri url, {
    String? body,
    Map<String, String>? headers,
  }) async {
    final request = await _http.openUrl(method, url);
    request.headers.set(
      'Authorization',
      'Basic ${base64Encode(utf8.encode('$username:$password'))}',
    );
    if (body != null) {
      final bytes = utf8.encode(body);
      request.headers.set('Content-Type', 'application/json; charset=utf-8');
      request.headers.contentLength = bytes.length;
    }
    for (final entry in headers?.entries ?? const <MapEntry<String, String>>[]) {
      request.headers.set(entry.key, entry.value);
    }
    if (body != null) request.add(utf8.encode(body));
    return request.close();
  }

  /// 失败信息带上实际请求的路径：坚果云等服务要求地址包含 WebDAV 目录
  /// （如 https://dav.jianguoyun.com/dav/），路径不对时只报状态码很难定位。
  StateError _failure(int code, String action, Uri url) => code == 401 ||
          code == 403
      ? StateError('$action：账号或密码不正确（${url.host}）')
      : StateError('$action（HTTP $code · ${url.host}${url.path}）');

  Future<String> _readBody(
    HttpClientResponse response,
    String action,
  ) async {
    final builder = BytesBuilder(copy: false);
    await for (final chunk in response) {
      builder.add(chunk);
      if (builder.length > fileLimit) {
        await response.drain<void>();
        throw StateError('$action：响应超过 8 MiB 上限');
      }
    }
    final text = utf8.decode(builder.takeBytes());
    return text.startsWith('\uFEFF') ? text.substring(1) : text;
  }

  @override
  Future<void> ensureDirectory(Uri directory) async {
    final response = await _send('MKCOL', directory);
    await response.drain<void>();
    if ({201, 200, 405}.contains(response.statusCode)) return;
    throw _failure(response.statusCode, '创建同步目录失败', directory);
  }

  static const _propFindBody =
      '<?xml version="1.0" encoding="utf-8"?>'
      '<D:propfind xmlns:D="DAV:"><D:prop><D:getcontentlength/></D:prop></D:propfind>';

  @override
  Future<List<WebDavRemoteFile>> list(Uri directory) async {
    final response = await _send(
      'PROPFIND',
      directory,
      body: _propFindBody,
      headers: const {'Depth': '1'},
    );
    final text = await _readBody(response, '读取同步目录');
    if (!{207, 200}.contains(response.statusCode)) {
      throw _failure(response.statusCode, '读取同步目录失败', directory);
    }
    final files = <WebDavRemoteFile>[];
    for (final block in RegExp(
      r'<(?:[\w-]+:)?response\b[^>]*>([\s\S]*?)</(?:[\w-]+:)?response>',
    ).allMatches(text)) {
      final body = block.group(1)!;
      final href = RegExp(
        r'<(?:[\w-]+:)?href>([^<]+)<',
      ).firstMatch(body)?.group(1);
      if (href == null) continue;
      final uri = directory.resolve(href);
      final name = uri.pathSegments.isEmpty ? '' : uri.pathSegments.last;
      if (!name.endsWith('.json')) continue;
      final size = int.tryParse(
        RegExp(
          r'<(?:[\w-]+:)?getcontentlength>(\d+)<',
        ).firstMatch(body)?.group(1) ?? '',
      );
      files.add(WebDavRemoteFile(name: name, uri: uri, bytes: size ?? -1));
    }
    return files;
  }

  @override
  Future<String> read(Uri file) async {
    final response = await _send('GET', file);
    if (response.statusCode != 200) {
      await response.drain<void>();
      throw _failure(response.statusCode, '下载同步文件失败', file);
    }
    if (response.contentLength > fileLimit) {
      await response.drain<void>();
      throw StateError('远端同步文件超过 8 MiB 上限');
    }
    return _readBody(response, '下载同步文件');
  }

  @override
  Future<void> write(Uri file, String content) async {
    final response = await _send('PUT', file, body: content);
    await response.drain<void>();
    if (!{200, 201, 204}.contains(response.statusCode)) {
      throw _failure(response.statusCode, '上传同步文件失败', file);
    }
  }
}

class WebDavSyncOutcome {
  const WebDavSyncOutcome({
    this.added = 0,
    this.updated = 0,
    this.removed = 0,
    this.conflicts = 0,
    this.sourcePrefs = 0,
    this.sourceImports = 0,
    this.warnings = const [],
    this.savedAt,
  });
  final int added;
  final int updated;
  final int removed;
  final int conflicts;

  /// 本次同步发生变化的片源偏好（收藏 / 隐藏）条目数。
  final int sourcePrefs;

  /// 本次同步从对端自动导入的片源数量。
  final int sourceImports;

  /// 非致命警告（如个别片源导入失败、粘贴导入的脚本源无法同步）。
  final List<String> warnings;
  final DateTime? savedAt;
  String get label {
    final base = '新增 $added · 更新 $updated · 移除 $removed · 冲突 $conflicts';
    final extra = <String>[
      if (sourcePrefs > 0) '片源偏好 $sourcePrefs 项',
      if (sourceImports > 0) '导入片源 $sourceImports 个',
    ];
    return extra.isEmpty ? base : '$base · ${extra.join(' · ')}';
  }

  bool get hasChanges =>
      added + updated + removed + sourcePrefs + sourceImports > 0;
}

class WebDavSynchronizer {
  WebDavSynchronizer({
    required this.store,
    required this.transport,
    this.repository,
    DateTime Function()? clock,
  }) : _clock = clock ?? DateTime.now;

  static const schema = 1;

  final LocalStore store;
  final WebDavTransport transport;

  /// 片源仓库：同步「片源订阅 + 收藏/隐藏状态」时用来列出已导入源、
  /// 自动导入对端设备新增的 LibreTV / drpy 源。为空（纯记录同步场景）
  /// 时跳过订阅收集与自动导入，只同步追剧记录。
  final AppRepository? repository;
  final DateTime Function() _clock;

  Future<WebDavSyncOutcome> synchronize() async {
    final settings = store.webDavSettings;
    if (!settings.configured) {
      throw StateError('请先填写 WebDAV 服务器地址、账号和密码');
    }
    final directory = settings.directoryUri;
    if (directory == null) {
      throw StateError('同步地址无效：需要 http/https 地址，目录名仅限字母、数字、- 和 _');
    }
    if (store.locked) throw StateError('当前用户已锁定，请先解锁');
    final epoch = store.profileEpoch;
    final sources = store.sources.map((source) => source.id).toSet();
    if (sources.isEmpty) throw StateError('当前用户没有可同步的站源');
    final document = store.lanDocument;
    final localBase = document.hashFor(sources);
    final localRecords = <String, LanRecord>{
      for (final entry in document.records.entries)
        if (sources.contains(entry.value.drama.source)) entry.key: entry.value,
    };

    await transport.ensureDirectory(directory);
    final remotes = await transport.list(directory);

    final remoteRecords = <String, LanRecord>{};
    final remotePrefs = <String, LanSourcePref>{};
    final remoteSubs = <String, LanSourceSub>{};
    for (final remote in remotes) {
      if (remote.name == '${document.replica}.json') continue;
      if (remote.bytes > HttpWebDavTransport.fileLimit) continue;
      final text = await transport.read(remote.uri);
      if (utf8.encode(text).length > HttpWebDavTransport.fileLimit) continue;
      LanDocument incoming;
      try {
        final payload = Map<String, dynamic>.from(jsonDecode(text) as Map);
        if (payload['schema'] != schema || payload['document'] is! Map) {
          throw const FormatException('同步文件格式无效');
        }
        incoming = LanDocument.fromJson(payload['document']);
      } on FormatException {
        continue;
      }
      for (final entry in incoming.records.entries) {
        if (!sources.contains(entry.value.drama.source)) continue;
        final existing = remoteRecords[entry.key];
        remoteRecords[entry.key] =
            existing == null ? entry.value : existing.merge(entry.value);
      }
      // 片源偏好多设备合并：同一源 ID 的对端记录也按最新时间戳归并，
      // 保证上传前拿到的是「所有对端设备的最新共识」。
      for (final entry in incoming.sourcePrefs.entries) {
        final existing = remotePrefs[entry.key];
        remotePrefs[entry.key] = existing == null
            ? entry.value
            : LanSourcePref.merge(existing, entry.value);
      }
      for (final entry in incoming.sourceSubs.entries) {
        final existing = remoteSubs[entry.key];
        remoteSubs[entry.key] = existing == null
            ? entry.value
            : LanSourceSub.merge(existing, entry.value);
      }
    }

    final result = <String, LanRecord>{};
    for (final id in {...localRecords.keys, ...remoteRecords.keys}) {
      final local = localRecords[id];
      final remote = remoteRecords[id];
      result[id] = local == null
          ? remote!
          : remote == null
          ? local
          : local.merge(remote);
    }
    if (result.length > LanDocument.limit) {
      throw StateError('合并后的追剧记录超过上限，请先整理记录');
    }

    await store.applyLanRecords(
      operation: lanID(),
      base: localBase,
      sources: sources,
      records: result.values,
      epoch: epoch,
      isCurrent: () => store.profileEpoch == epoch && !store.locked,
    );

    // ---- 片源订阅与收藏 / 隐藏状态 ----
    final localPrefs = store.sourcePrefsSnapshot();
    final localSubs = await _collectLocalSubs();
    final mergedPrefs = <String, LanSourcePref>{
      for (final id in {...localPrefs.keys, ...remotePrefs.keys})
        id: remotePrefs[id] == null
            ? localPrefs[id]!
            : localPrefs[id] == null
            ? remotePrefs[id]!
            : LanSourcePref.merge(localPrefs[id]!, remotePrefs[id]!),
    };
    final mergedSubs = <String, LanSourceSub>{
      for (final id in {...localSubs.keys, ...remoteSubs.keys})
        id: remoteSubs[id] == null
            ? localSubs[id]!
            : localSubs[id] == null
            ? remoteSubs[id]!
            : LanSourceSub.merge(localSubs[id]!, remoteSubs[id]!),
    };
    final prefsChanged = await store.applySourcePrefs(mergedPrefs);
    final imports = await _autoImportSubs(mergedSubs);

    final savedAt = _clock.call().toUtc();
    try {
      final merged = store.lanDocument;
      final payload = jsonEncode({
        'schema': schema,
        'savedAt': savedAt.toIso8601String(),
        'document': LanDocument(
          replica: merged.replica,
          counter: merged.counter,
          records: merged.records,
          sourcePrefs: mergedPrefs,
          sourceSubs: mergedSubs,
        ).toJson(),
      });
      if (utf8.encode(payload).length > HttpWebDavTransport.fileLimit) {
        throw StateError('同步文件超过 8 MiB 上限，请先清理追剧记录');
      }
      await transport.write(directory.resolve('${merged.replica}.json'), payload);
    } catch (failure) {
      _record(savedAt: savedAt, error: '已合并保存到本机，上传失败：$failure');
      rethrow;
    }
    _record(savedAt: savedAt);
    final count = LanChangeCount.between(localRecords, result);
    return WebDavSyncOutcome(
      added: count.added,
      updated: count.updated,
      removed: count.removed,
      conflicts: count.conflicts,
      sourcePrefs: prefsChanged,
      sourceImports: imports.imported,
      warnings: imports.warnings,
      savedAt: savedAt,
    );
  }

  /// 收集本机已导入的 LibreTV / drpy 片源为订阅条目。
  ///
  /// 导入时间原生核心不记录，统一按 at=0 处理：对端条目（带同步时间戳）
  /// 在名称 / 地址上一律更新，本机条目只负责补全并集。drpy 粘贴导入的
  /// 脚本源没有 scriptUrl、无法跨设备重建，直接跳过。
  /// repository 为空或核心不支持时返回空表（只同步追剧记录）。
  Future<Map<String, LanSourceSub>> _collectLocalSubs() async {
    final repository = this.repository;
    if (repository == null) return {};
    final subs = <String, LanSourceSub>{};
    try {
      for (final LibreTVSite site in await repository.libretvSites()) {
        if (site.key.isEmpty || site.api.trim().isEmpty) continue;
        subs[site.sourceId] = LanSourceSub(name: site.name, api: site.api, at: 0);
      }
    } catch (_) {}
    try {
      for (final DrpySite script in await repository.drpySites()) {
        if (script.key.isEmpty || script.scriptUrl.trim().isEmpty) continue;
        subs[script.sourceId] = LanSourceSub(
          name: script.name,
          api: script.scriptUrl,
          at: 0,
        );
      }
    } catch (_) {}
    return subs;
  }

  /// 把对端设备导入、本机还没有的片源自动导入（幂等：已注册的源 ID 跳过）。
  ///
  /// 失败仅收集警告、不中断同步——用户确认过「同步时自动导入」策略：
  /// 个别源失效不应阻塞追剧记录同步。导入成功后刷新 SourceSite 运行时
  /// 注册表并广播，让新片源立即可用（与启动时 _syncRuntimeSources 同序）。
  Future<({int imported, List<String> warnings})> _autoImportSubs(
    Map<String, LanSourceSub> subs,
  ) async {
    final repository = this.repository;
    if (repository == null || subs.isEmpty) {
      return (imported: 0, warnings: const <String>[]);
    }
    final warnings = <String>[];
    var imported = 0;
    for (final entry in subs.entries) {
      final id = entry.key;
      if (SourceSite.isKnown(id)) continue;
      final sub = entry.value;
      final label = sub.name.trim().isEmpty ? id : sub.name.trim();
      try {
        if (id.startsWith('libretv:')) {
          if (sub.api.trim().isEmpty) {
            warnings.add('片源「$label」缺少采集地址，已跳过');
            continue;
          }
          await repository.libretvImport(sub.api);
        } else if (id.startsWith('drpy:')) {
          if (sub.api.trim().isEmpty) {
            warnings.add('脚本源「$label」为粘贴导入，无法跨设备同步');
            continue;
          }
          await repository.drpyImport(sub.api);
        } else {
          continue;
        }
        imported++;
      } catch (failure) {
        warnings.add('导入片源「$label」失败：$failure');
      }
    }
    if (imported > 0) {
      try {
        final sites = await repository.libretvSites();
        SourceSite.setRuntimeSources([
          for (final site in sites)
            if (site.key.isNotEmpty) site.toSourceSite(),
        ]);
        final scripts = await repository.drpySites();
        SourceSite.setRuntimeSources([
          ...SourceSite.runtimeValues,
          for (final script in scripts)
            if (script.key.isNotEmpty) script.toSourceSite(),
        ]);
      } catch (_) {}
      store.notifySourceRegistryChanged();
    }
    return (imported: imported, warnings: warnings);
  }

  void _record({required DateTime savedAt, String? error}) {
    final next = store.webDavSettings.copyWith(
      lastSyncAt: savedAt,
      lastError: error,
      clearLastError: error == null,
    );
    unawaited(store.saveWebDavSettings(next).catchError((Object _) {}));
  }
}
