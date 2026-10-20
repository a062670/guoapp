import 'models.dart';

enum PlaybackRecoveryAction { alternative, refresh, stop }

/// 自动恢复策略：先把这一集的其他线路都试一遍（本地切换，不重新解析），
/// 全部试过之后再重新解析一次（可能是播放凭证过期），最后才报错。
///
/// 线路按环形处理：记忆停在最后一条时，下一条回到首选线路；但如果下一条
/// 已经试过（说明是绕回来的），就不再重复，直接进入刷新或停止，避免在同
/// 一组坏线之间打转，也避免多余的请求。
class PlaybackRecovery {
  static const maxAttempts = 3;
  int _attempts = 0;
  bool _refreshed = false;
  final Set<int> _tried = {};

  void reset() {
    _attempts = 0;
    _refreshed = false;
    _tried.clear();
  }

  PlaybackRecoveryAction next(PlaybackPlan plan) {
    if (_attempts >= maxAttempts) {
      return PlaybackRecoveryAction.stop;
    }
    _tried.add(plan.routeIndex);
    final nextRoute = plan.routeCount > 1
        ? (plan.routeIndex + 1) % plan.routeCount
        : plan.routeIndex;
    if (plan.hasAlternative && !_tried.contains(nextRoute)) {
      _attempts++;
      return PlaybackRecoveryAction.alternative;
    }
    if (!_refreshed) {
      _attempts++;
      _refreshed = true;
      // 重新解析后线路可能已经刷新（凭证/地址变化），允许再把各条线试一遍。
      _tried.clear();
      return PlaybackRecoveryAction.refresh;
    }
    return PlaybackRecoveryAction.stop;
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
