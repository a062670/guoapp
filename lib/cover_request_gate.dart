import 'dart:async';
import 'dart:collection';

/// 封面请求闸门：限制并发 + 同一张封面只发一次。
///
/// 为什么需要：
///   - 「全部站源」流式刷新时列表会不断追加新卡片，每个新卡片都会请求一次
///     封面。没有上限时，几十个请求同时压到 FFI/Go，既抢带宽也抢 Go 侧
///     仅 4 个的封面下载槽位，加载期间整体更卡；
///   - 同一个封面可能被多处同时请求（列表、详情、下载页、重试），合流后
///     只发一次，失败结果也会共享（失败的卡片有自己的重试入口）。
///
/// 只是「排队」：容量满了就等，不丢请求、不改请求顺序（FIFO）。
class CoverRequestGate {
  CoverRequestGate({this.concurrency = 4}) : assert(concurrency > 0);

  /// 同时在飞的封面请求上限。
  final int concurrency;

  int _active = 0;
  final Queue<Completer<void>> _waiters = Queue<Completer<void>>();
  final Map<String, Future<String>> _flights = {};

  /// 当前在飞数量（测试用）。
  int get active => _active;

  /// 同一个 key 的请求合流。返回的 Future 完成或失败后该 key 自动释放，
  /// 之后可以重新请求（重试路径依赖这一点）。
  Future<String> load(String key, Future<String> Function() action) {
    final existing = _flights[key];
    if (existing != null) return existing;
    final future = _schedule(action);
    _flights[key] = future;
    // 用 whenComplete 清理；注意不能把 whenComplete 的结果存回 map，
    // 否则并发等待者拿到的会是另一条 Future 链。
    // 这里必须吞掉派生 Future 的错误：真正的错误由返回给调用方的 future
    // 传递，派生链上没人 await，未处理错误会冒到 Zone（测试里直接失败）。
    future.whenComplete(() {
      if (identical(_flights[key], future)) _flights.remove(key);
    }).ignore();
    return future;
  }

  Future<T> _schedule<T>(Future<T> Function() action) async {
    await _acquire();
    try {
      return await action();
    } finally {
      _release();
    }
  }

  Future<void> _acquire() {
    if (_active < concurrency) {
      _active++;
      return Future<void>.value();
    }
    final waiter = Completer<void>();
    _waiters.add(waiter);
    return waiter.future;
  }

  void _release() {
    if (_waiters.isNotEmpty) {
      // 槽位直接转交给下一个等待者：_active 不变。
      _waiters.removeFirst().complete();
      return;
    }
    _active--;
  }
}
