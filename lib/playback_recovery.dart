import 'models.dart';

enum PlaybackRecoveryAction { alternative, refresh, stop }

/// 自动恢复策略：先切换这一集的其他线路（本地切换，不重新解析，最多 3 次），
/// 再重新解析（最多 3 次，可能是播放凭证过期或源站暂时拒绝），最后才报错。
///
/// 线路按环形处理：记忆停在最后一条时，下一条回到首选线路；但如果下一条
/// 已经试过（说明是绕回来的），就不再重复，直接进入刷新或停止，避免在同
/// 一组坏线之间打转，也避免多余的请求。
class PlaybackRecovery {
  static const maxAlternatives = 3;
  static const maxRefreshes = 3;
  int _alternatives = 0;
  int _refreshes = 0;
  final Set<int> _tried = {};

  int get refreshes => _refreshes;

  Duration get refreshDelay => Duration(seconds: _refreshes);

  void reset() {
    _alternatives = 0;
    _refreshes = 0;
    _tried.clear();
  }

  bool refresh() {
    if (_refreshes >= maxRefreshes) return false;
    _refreshes++;
    _tried.clear();
    return true;
  }

  PlaybackRecoveryAction next(PlaybackPlan plan) {
    _tried.add(plan.routeIndex);
    final nextRoute = plan.routeCount > 1
        ? (plan.routeIndex + 1) % plan.routeCount
        : plan.routeIndex;
    if (plan.hasAlternative &&
        !_tried.contains(nextRoute) &&
        _alternatives < maxAlternatives) {
      _alternatives++;
      return PlaybackRecoveryAction.alternative;
    }
    return refresh()
        ? PlaybackRecoveryAction.refresh
        : PlaybackRecoveryAction.stop;
  }
}

class PlaybackHealth {
  static const stallTimeout = Duration(seconds: 20);
  Duration? _position;
  DateTime? _lastProgress;

  void reset() {
    _position = null;
    _lastProgress = null;
  }

  bool stalled({
    required Duration position,
    required bool playing,
    required bool foreground,
    required DateTime now,
  }) {
    if (!playing || !foreground || position != _position) {
      _position = position;
      _lastProgress = now;
      return false;
    }
    _lastProgress ??= now;
    return now.difference(_lastProgress!) >= stallTimeout;
  }
}
