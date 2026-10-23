import 'dart:async';

import 'package:flutter/foundation.dart';

import 'core_bridge.dart';
import 'local_store.dart';
import 'models.dart';
import 'source_status.dart';

class LibraryUpdater extends ChangeNotifier {
  LibraryUpdater(this.repository, this.store, {this.onCatalogChanged})
    : _epoch = store.profileEpoch;

  final AppRepository repository;
  final LocalStore store;
  final ValueChanged<String>? onCatalogChanged;
  final int _epoch;
  final _statuses = <String, SourceStatus>{};
  final _errors = <String, String>{};
  final _pending = <String>{};
  final _stopRequested = <String>{};
  final _revisions = <String, int>{};
  final _delivered = <String, String>{};

  /// 已记入健康度账本的「任务完成」修订号。
  ///
  /// 状态是轮询出来的，同一个完成状态会被反复读到；没有这个去重，
  /// 一次失败会被记成几十次连续失败，源就永远处于跳过冷却里。
  final _healthDelivered = <String, String>{};
  Timer? _timer;
  bool _polling = false;
  bool _disposed = false;
  int _ticks = 0;

  /// 上次通知监听者时的状态签名；状态没变就不再通知，避免首页被
  /// 周期轮询（每 10 秒一次）反复全量重建——这是操作卡顿的主因。
  String _notifiedSignature = '';

  bool get _valid =>
      !_disposed && !store.locked && _epoch == store.profileEpoch;
  SourceStatus? status(String source) => _statuses[source];
  String error(String source) => _errors[source] ?? '';
  bool busy(String source) =>
      _pending.contains(source) || _statuses[source]?.running == true;

  void startWatching() {
    if (_timer != null || !repository.supportsSourceManagement || !_valid) {
      return;
    }
    unawaited(refresh());
    _timer = Timer.periodic(const Duration(seconds: 2), (_) {
      _ticks++;
      if (_statuses.values.any((status) => status.running) || _ticks % 5 == 0) {
        unawaited(refresh());
      }
    });
  }

  /// 状态签名：任一源的状态 / 错误 / 进行中 / 停止请求变化都会改变它。
  String _signature() {
    final keys = _statuses.keys.toList()..sort();
    final parts = <String>[
      for (final key in keys)
        '$key|${_statuses[key]!.operation}|${_statuses[key]!.startedAt}|'
            '${_statuses[key]!.finishedAt}|${_statuses[key]!.updatedAt}|'
            '${_statuses[key]!.count}|${_statuses[key]!.running}|'
            '${_statuses[key]!.storageError}',
      for (final key in _errors.keys.toList()..sort()) '$key!${_errors[key]}',
      for (final key in _pending.toList()..sort()) '+$key',
      for (final key in _stopRequested.toList()..sort()) '-$key',
    ];
    return parts.join(';');
  }

  void _notify() {
    if (!_valid) return;
    final signature = _signature();
    if (signature == _notifiedSignature) return;
    _notifiedSignature = signature;
    notifyListeners();
  }

  void _accept(SourceStatus status, {bool submitted = false}) {
    if (!_valid || !store.allowsSource(status.source)) return;
    _statuses[status.source] = status;
    _errors.remove(status.source);
    final revision =
        '${status.operation}|${status.startedAt}|${status.finishedAt}|'
        '${status.updatedAt}|${status.count}|${status.storageError}';
    if (!status.running &&
        status.operation == 'update' &&
        _healthDelivered[status.source] != revision) {
      _healthDelivered[status.source] = revision;
      final started = status.startedAt, finished = status.finishedAt;
      store.noteSourceHealth(
        status.source,
        ok: status.error.isEmpty,
        elapsedMs: started != null && finished != null
            ? finished.difference(started).inMilliseconds
            : 0,
      );
    }
    if (!status.running &&
        (status.finishedAt != null || submitted) &&
        {
          'update',
          'more',
          'metadata',
          'vipMetadata',
          'retrySave',
        }.contains(status.operation) &&
        _delivered[status.source] != revision) {
      _delivered[status.source] = revision;
      onCatalogChanged?.call(status.source);
    }
  }

  Future<void> refresh() async {
    if (_polling || !_valid || !repository.supportsSourceManagement) return;
    _polling = true;
    try {
      // 一次拿全部状态（原生实现）。几十个源逐个 RPC 会在启动/轮询时
      // 连续启动几十个 isolate；拿不到批量结果时退回逐源查询。
      var batch = const <String, SourceStatus>{};
      try {
        batch = await repository.sourceStatuses();
      } catch (_) {}
      for (final source in store.sources) {
        if (!_valid) return;
        final revision = _revisions[source.id] ?? 0;
        try {
          final status = batch.isNotEmpty
              ? batch[source.id] ?? SourceStatus.fromJson({'source': source.id})
              : await repository.sourceStatus(source.id);
          if (_valid && revision == (_revisions[source.id] ?? 0)) {
            _accept(status);
          }
        } catch (error) {
          if (_valid && revision == (_revisions[source.id] ?? 0)) {
            _errors[source.id] = error.toString();
          }
        }
      }
    } finally {
      _polling = false;
      _notify();
    }
  }

  Future<void> update(Iterable<SourceSite> sources) async {
    if (!_valid || !repository.supportsSourceManagement) return;
    final selected = store.sourceHealth.order(
      sources
          .map((source) => source.id)
          .toSet()
          .where((source) => store.allowsSource(source) && !busy(source)),
    );
    for (final source in selected) {
      _stopRequested.remove(source);
      _pending.add(source);
      _errors.remove(source);
      _revisions[source] = (_revisions[source] ?? 0) + 1;
    }
    _notify();
    try {
      for (final source in selected) {
        if (!_valid || !store.allowsSource(source)) break;
        try {
          if (_stopRequested.contains(source)) continue;
          final status = await repository.startSourceJob(source, 'update');
          _accept(status, submitted: true);
          if (_valid && _stopRequested.contains(source) && status.running) {
            _accept(await repository.cancelSourceJob(source));
          }
        } catch (error) {
          if (_valid) _errors[source] = error.toString();
        } finally {
          _pending.remove(source);
          _notify();
        }
      }
    } finally {
      _pending.removeAll(selected);
      _notify();
    }
  }

  Future<void> stop(Iterable<SourceSite> sources) async {
    final selected = sources.toList();
    if (!_valid) return;
    for (final source in selected) {
      if (store.allowsSource(source.id)) _stopRequested.add(source.id);
    }
    for (final source in selected) {
      if (!_valid) return;
      if (!store.allowsSource(source.id) ||
          _pending.contains(source.id) ||
          _statuses[source.id]?.running != true) {
        continue;
      }
      _pending.add(source.id);
      _revisions[source.id] = (_revisions[source.id] ?? 0) + 1;
      _notify();
      try {
        _accept(await repository.cancelSourceJob(source.id));
      } catch (error) {
        if (_valid) _errors[source.id] = error.toString();
      } finally {
        _pending.remove(source.id);
        _notify();
      }
    }
  }

  @override
  void dispose() {
    _disposed = true;
    _timer?.cancel();
    super.dispose();
  }
}
