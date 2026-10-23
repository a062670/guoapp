import 'dart:convert';

/// 单个源的「健康度」记录。
///
/// 只记三件事，够用且不会无限膨胀：
///   - [failures]：**连续**失败次数，成功一次立即归零；
///   - [avgMs]：最近若干次成功的耗时指数滑动平均（EWMA，0.7/0.3）；
///   - [lastOkAt] / [lastFailAt]：最近一次成功 / 失败的时间戳（毫秒）。
///
/// 不记录每次请求的明细、不做百分位统计——那是调试工具的事。刷新时要的只有
/// 「这个源现在还能不能用、大概多慢」两个判断。
class SourceHealthStat {
  SourceHealthStat({
    this.failures = 0,
    this.avgMs = 0,
    this.samples = 0,
    this.lastOkAt = 0,
    this.lastFailAt = 0,
  });

  int failures;
  double avgMs;
  int samples;
  int lastOkAt;
  int lastFailAt;

  /// 有过成功记录（[avgMs] 才有意义）。
  bool get known => samples > 0;
  bool get failing => failures > 0;

  Map<String, dynamic> toJson() => {
    if (failures > 0) 'f': failures,
    if (samples > 0) 'a': avgMs.round(),
    if (samples > 0) 'n': samples,
    if (lastOkAt > 0) 'ok': lastOkAt,
    if (lastFailAt > 0) 'fail': lastFailAt,
  };

  factory SourceHealthStat.fromJson(Map<String, dynamic> json) => SourceHealthStat(
    failures: _int(json['f']),
    avgMs: _int(json['a']).toDouble(),
    samples: _int(json['n']),
    lastOkAt: _int(json['ok']),
    lastFailAt: _int(json['fail']),
  );

  static int _int(Object? value) =>
      value is int ? value : value is num ? value.toInt() : 0;
}

/// 一次刷新里「按健康度排好序 + 该跳过谁」的结果。
class SourceHealthPlan {
  const SourceHealthPlan(this.order, this.skipped);
  final List<String> order;
  final List<String> skipped;
}

/// 源级健康度账本。
///
/// ## 为什么需要它
///
/// 流式刷新解决的是「用户多久能看见第一批内容」，解决不了两件事：
///   1. 死源占着并发槽位——「全部站源」可能有几十个源、并发只有 8 个，
///      前排混进 3 个死源，后面健康源就要多等一整轮超时；
///   2. 尾部拖影——总条数、分类聚合、搜索收敛都要等**最后一个**源返回，
///      而最后一个恰好总是那个超时的死源。
///
/// 所以这里只做两件很小的事：**把连续失败的源排到队尾**，以及在冷却期内
/// **暂时跳过**它。不删除、不隐藏、不改任何持久化的站源配置——用户切到
/// 这个源单独浏览时照常请求，手动刷新也会立刻重试。
///
/// ## 判定规则（保持保守）
///
///   - 连续失败 >= [failureThreshold] 且在 [cooldown] 内 → 跳过；
///   - 排序：健康的（按平均耗时升序）→ 没记录的 → 失败的（失败次数多的靠后）；
///   - 排序稳定：同档内保持调用方给的原始顺序。
class SourceHealthBook {
  SourceHealthBook({DateTime Function()? now}) : _now = now ?? DateTime.now;

  SourceHealthBook.fromJson(String? raw, {DateTime Function()? now})
    : _now = now ?? DateTime.now {
    _entries.addAll(_decode(raw));
  }

  /// 连续失败多少次后进入冷却。
  static const failureThreshold = 2;

  /// 冷却时长：到期后自动再试一次，不需要用户手动操作。
  static const cooldown = Duration(minutes: 10);

  /// 账本条目上限。导入源删了又加会留下孤儿记录，超出时先丢最久没动静的。
  static const maxEntries = 200;

  static const _version = 1;

  final DateTime Function() _now;
  final Map<String, SourceHealthStat> _entries = {};

  int get length => _entries.length;
  bool get isEmpty => _entries.isEmpty;

  SourceHealthStat? healthOf(String source) => _entries[source];

  /// 排序 + 跳过。`force`（用户手动刷新 / 单源浏览）时不跳过。
  SourceHealthPlan plan(
    Iterable<String> sources, {
    bool force = false,
    bool allowSkip = true,
  }) {
    final list = sources.toList();
    final now = _now().millisecondsSinceEpoch;
    final skipped = <String>[];
    final kept = <String>[];
    for (final source in list) {
      if (allowSkip && !force && shouldSkip(source, now: now)) {
        skipped.add(source);
      } else {
        kept.add(source);
      }
    }
    return SourceHealthPlan(order(kept), skipped);
  }

  /// 是否处于「连续失败冷却」中。
  bool shouldSkip(String source, {int? now}) {
    final entry = _entries[source];
    if (entry == null || entry.failures < failureThreshold) return false;
    final at = now ?? _now().millisecondsSinceEpoch;
    // 时钟回拨（用户改系统时间）时不要把源永久锁死：时间戳比现在还晚超过
    // 一个冷却周期，就当作已过期。
    if (entry.lastFailAt <= 0) return false;
    return at - entry.lastFailAt < cooldown.inMilliseconds &&
        entry.lastFailAt - at < cooldown.inMilliseconds;
  }

  /// 按健康度稳定排序：健康（快→慢）→ 未知 → 失败（失败多→少）。
  List<String> order(Iterable<String> sources) {
    final list = sources.toList();
    // List.sort 不稳定，用原始下标做最终 tie-break，保证同档内顺序不变。
    final decorated = [
      for (var index = 0; index < list.length; index++)
        (index: index, source: list[index]),
    ];
    decorated.sort((a, b) {
      final rankA = _rank(a.source), rankB = _rank(b.source);
      if (rankA != rankB) return rankA - rankB;
      if (rankA == 0) {
        final byLatency = _entries[a.source]!.avgMs.compareTo(
          _entries[b.source]!.avgMs,
        );
        if (byLatency != 0) return byLatency;
      } else if (rankA == 2) {
        final byFailures = _entries[b.source]!.failures.compareTo(
          _entries[a.source]!.failures,
        );
        if (byFailures != 0) return byFailures;
      }
      return a.index - b.index;
    });
    return [for (final entry in decorated) entry.source];
  }

  int _rank(String source) {
    final entry = _entries[source];
    if (entry == null) return 1;
    // 有失败记录就归到「失败」档，哪怕一次都没成功过（死源通常如此）。
    if (entry.failing) return 2;
    if (!entry.known) return 1;
    return 0;
  }

  /// 记录一次请求结果。[elapsedMs] 只在成功时参与平均耗时。
  ///
  /// 返回账本是否发生变化——调用方据此决定要不要落盘，避免每个源都写一次。
  bool record(String source, {required bool ok, required int elapsedMs}) {
    if (source.isEmpty) return false;
    final now = _now().millisecondsSinceEpoch;
    final entry = _entries.putIfAbsent(source, SourceHealthStat.new);
    if (ok) {
      final sample = elapsedMs < 0 ? 0 : elapsedMs;
      entry.avgMs = entry.samples == 0
          ? sample.toDouble()
          : entry.avgMs * 0.7 + sample * 0.3;
      entry.samples = (entry.samples + 1).clamp(1, 50);
      entry.failures = 0;
      entry.lastOkAt = now;
    } else {
      entry.failures++;
      entry.lastFailAt = now;
    }
    _prune();
    return true;
  }

  /// 某个源的缓存被清空 / 用户手动重试时清除记录。
  void reset(String source) => _entries.remove(source);

  void _prune() {
    if (_entries.length <= maxEntries) return;
    final entries = _entries.entries.toList()
      ..sort((a, b) {
        final at = a.value.lastOkAt > a.value.lastFailAt
            ? a.value.lastOkAt
            : a.value.lastFailAt;
        final bt = b.value.lastOkAt > b.value.lastFailAt
            ? b.value.lastOkAt
            : b.value.lastFailAt;
        return at.compareTo(bt);
      });
    for (var index = 0; index < entries.length - maxEntries; index++) {
      _entries.remove(entries[index].key);
    }
  }

  String toJson() {
    if (_entries.isEmpty) return '';
    return jsonEncode({
      'v': _version,
      'sources': {
        for (final entry in _entries.entries) entry.key: entry.value.toJson(),
      },
    });
  }

  static Map<String, SourceHealthStat> _decode(String? raw) {
    if (raw == null || raw.isEmpty) return {};
    try {
      final root = jsonDecode(raw);
      if (root is! Map) return {};
      final sources = root['sources'];
      if (sources is! Map) return {};
      final result = <String, SourceHealthStat>{};
      for (final entry in sources.entries) {
        final key = entry.key;
        final value = entry.value;
        if (key is! String || key.isEmpty || value is! Map) continue;
        result[key] = SourceHealthStat.fromJson(
          Map<String, dynamic>.from(value),
        );
      }
      return result;
    } catch (_) {
      // 账本只是加速用的旁路数据，损坏时直接当空账本，绝不阻塞启动。
      return {};
    }
  }
}
