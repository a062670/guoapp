import 'dart:async';
import 'dart:math' as math;

import 'package:flutter/gestures.dart';
import 'package:flutter/services.dart';
import 'package:flutter/widgets.dart';
import 'package:media_kit/media_kit.dart';

import 'app_layout.dart';
import 'widgets.dart';

// [手势分区·2026-10] 左半屏垂直=亮度；右半屏水平=进度调整（新增），
// 右半屏垂直=刷剧切集（保留，方向区分互不干扰）。
enum SwipeAction { none, brightness, episode, progress }

class GestureHudState {
  const GestureHudState({this.type = SwipeAction.none, this.value = 0.0});
  final SwipeAction type;
  final double value; // 0.0 ~ 1.0
}

class PlayerInteractions extends ChangeNotifier {
  PlayerInteractions({
    required this.player,
    required this.available,
    required this.baseSpeed,
    required this.holdSpeed,
    required this.onTogglePlayback,
    required this.onFullscreen,
    required this.onEpisode,
    this.onSeek,
  }) {
    _playing = player.stream.playing.listen((playing) {
      if (!playing) cancel();
    });
    AppDevice.getBrightness()
        .then((val) {
          if (!_disposed) {
            _brightness = val;
            notifyListeners();
          }
        })
        .catchError((_) {});
  }

  final Player player;
  final bool Function() available;
  final double Function() baseSpeed;
  final double Function() holdSpeed;
  final VoidCallback onTogglePlayback;
  final VoidCallback onFullscreen;
  final String Function(int direction) onEpisode;
  final Future<void> Function(Duration)? onSeek;
  late final StreamSubscription<bool> _playing;
  Timer? _holdTimer;
  Timer? _hintTimer;
  Future<void> _rates = Future<void>.value();
  final Set<int> _pointers = {};
  int? _pointer;
  Offset? _origin;
  Offset? _lastPosition;
  Duration _started = Duration.zero;
  double _swipeThreshold = 70;
  bool _swipeEnabled = false;
  bool _moved = false;
  bool _held = false;
  bool _boosting = false;
  bool _keyboardHold = false;
  bool _cancelUntilRelease = false;
  bool _disposed = false;
  bool _boostRestoring = false;
  bool _progressPreviewing = false;
  double _unmutedVolume = 100;
  String _feedback = '';
  bool _compact = false;
  DateTime _ignoreTapUntil = DateTime(2000);
  SwipeAction _swipeAction = SwipeAction.none;
  GestureHudState _hudState = const GestureHudState();
  double _brightness = 0.5;
  double _initialBrightness = 0.5;

  /// 亮度手势是否已确认（垂直拖动超过阈值）。
  /// 按下只算「候选区域」，未确认前不改亮度、不显示 HUD——
  /// 否则左半屏长按倍速 / 双击暂停都会先闪一个亮度图标。
  bool _brightnessLatched = false;

  /// latch 时刻的 y 坐标，作为亮度增量基准（避免从阈值点开始跳变）。
  double _brightnessAnchorDy = 0;
  double _viewWidth = 0.0;
  double _viewHeight = 0.0;
  int _seekSeconds = 0;
  Timer? _hudTimer;

  /// 右半屏水平拖动 → 进度调整量：拖动满屏宽 ≈ ±90 秒（常见播放器量级）。
  static const double _kProgressFullWidthSeconds = 90;

  /// 把水平拖动像素换算成秒（正=快进，负=后退）。
  int _progressSeconds(double dx) => _viewWidth <= 0
      ? 0
      : (dx / _viewWidth * _kProgressFullWidthSeconds).round();

  /// 拖动过程中的实时目标位置提示（与 seek() 的提示文案保持一致）。
  String _seekHint(int seconds) {
    if (player.state.duration <= Duration.zero) return '';
    final target = (player.state.position.inMilliseconds + seconds * 1000)
        .clamp(0, player.state.duration.inMilliseconds);
    return '${seconds > 0 ? '快进至' : '后退至'} ${formatPosition(target / 1000)}';
  }

  GestureHudState get hudState => _hudState;
  double get brightness => _brightness;
  bool get isBrightnessActive => _hudState.type == SwipeAction.brightness;

  /// 进程态提示（倍速 boost 中/恢复、右半屏进度拖动预览）期间不唤出
  /// 播放器 chrome（暂停键/进度条）。这些提示走 feedback 通道展示toast，
  /// 会被 player_controls 的 _interactionChanged 误判为「该显示控件了」，
  /// 导致长按倍速时屏幕中间弹出暂停按钮、下方弹出进度条（+90 修复）。
  bool get chromeHintSuppressed =>
      _boosting || _boostRestoring || _progressPreviewing;

  void setBrightnessDirect(double value) {
    if (_disposed) return;
    _brightness = value.clamp(0.01, 1.0);
    AppDevice.setBrightness(_brightness);
    _showHud(SwipeAction.brightness, _brightness);
  }

  void dismissBrightnessHud() {
    _scheduleDismissHud();
  }

  String get feedback => _feedback;
  bool get boosting => _boosting;

  /// 倍速进行中与恢复倍速的提示是长按期间常驻的，用小号紧凑样式并靠顶部
  /// 呈现；切集、快进等一次性提示仍用常规尺寸，避免短提示看不清。
  bool get compactFeedback => _compact;
  bool get suppressTap => DateTime.now().isBefore(_ignoreTapUntil);
  Future<void> get pendingRates => _rates;

  String get _boostLabel => '${_speedText(holdSpeed())} 倍速 · 松开恢复';

  static String _speedText(double value) =>
      value == value.roundToDouble() ? value.toInt().toString() : '$value';

  void hint(String message, {bool persistent = false, bool compact = false}) {
    if (_disposed) return;
    _hintTimer?.cancel();
    if (_feedback != message || _compact != compact) {
      _feedback = message;
      _compact = compact;
      notifyListeners();
    }
    if (!persistent && message.isNotEmpty) {
      _hintTimer = Timer(const Duration(milliseconds: 1200), () {
        // 恢复倍速的提示到期即解除 chrome 抑制；仍在 boost 中的由
        // _boosting 继续抑制（持久提示不走进这个分支）。
        _boostRestoring = false;
        _progressPreviewing = false;
        hint(_boosting ? _boostLabel : '', persistent: true, compact: true);
      });
    }
  }

  Future<void> applySpeed() => _setRate(baseSpeed());

  Future<void> _setRate(double value) {
    _rates = _rates
        .catchError((Object _) {})
        .then((_) => player.setRate(value));
    unawaited(
      _rates.catchError((Object _) {
        hint('倍速调整失败，请重试');
      }),
    );
    return _rates;
  }

  void _beginHold({bool keyboard = false}) {
    if (!available() || _holdTimer != null || _boosting) return;
    _keyboardHold = keyboard;
    _holdTimer = Timer(const Duration(milliseconds: 350), () {
      _holdTimer = null;
      if (_disposed ||
          !available() ||
          !player.state.playing ||
          player.state.completed) {
        return;
      }
      _boosting = true;
      _held = true;
      unawaited(_setRate(holdSpeed()));
      hint(_boostLabel, persistent: true, compact: true);
    });
  }

  void _endHold({bool tap = false, bool silent = false}) {
    final wasKeyboard = _keyboardHold;
    final boosted = _boosting;
    _holdTimer?.cancel();
    _holdTimer = null;
    _keyboardHold = false;
    _boosting = false;
    if (boosted) {
      unawaited(_setRate(baseSpeed()));
      // 恢复提示同样属进程态，抑制 chrome（+90：长按倍速不应拉出控件）。
      _boostRestoring = true;
      if (!silent) {
        hint('恢复 ${_speedText(baseSpeed())} 倍速', compact: true);
      }
    } else if (tap && wasKeyboard) {
      seek(5);
    }
  }

  void cancel() {
    if (_disposed) return;
    if (_pointers.isNotEmpty) {
      _cancelUntilRelease = true;
      _ignoreTapUntil = DateTime.now().add(const Duration(milliseconds: 600));
    }
    _pointer = null;
    _origin = null;
    _lastPosition = null;
    _seekSeconds = 0;
    _boostRestoring = false;
    _progressPreviewing = false;
    _endHold(silent: true);
    hint('');
  }

  void pointerDown(
    PointerDownEvent event, {
    required bool swipeEnabled,
    double width = 0.0,
    required double height,
  }) {
    _pointers.add(event.pointer);
    if (_pointers.length != 1 || _cancelUntilRelease) {
      cancel();
      return;
    }
    if (!available() || event.buttons != kPrimaryButton) return;
    _pointer = event.pointer;
    _origin = _lastPosition = event.localPosition;
    _started = event.timeStamp;
    _swipeEnabled = swipeEnabled && event.kind == PointerDeviceKind.touch;
    _swipeThreshold = math.max(30, math.min(80, height * .08));
    _viewWidth = width;
    _viewHeight = height;
    _moved = _held = false;
    _seekSeconds = 0;
    _swipeAction = SwipeAction.none;

    if (_swipeEnabled && width > 0) {
      // 判定触摸落点：
      // 左侧 35% 区域 -> 竖向：红果风格亮度条；横向：进度调整
      // 右侧 65% 区域 -> 水平拖动调整进度，垂直滑动刷剧切集，绝对不调节音量
      // （水平/垂直在 pointerMove 里按主方向 latch，二者互斥）
      if (event.localPosition.dx < width * 0.35) {
        // 只是「候选」亮度区，**不在这里弹 HUD**：
        // 左半屏长按（倍速）与双击（暂停）在按下瞬间就命中此处，
        // 旧实现直接 _showHud，导致这两个操作先闪一个亮度图标。
        // 真正确认是垂直拖动后，才在 pointerMove 里 latch 并显示。
        _swipeAction = SwipeAction.brightness;
        _brightnessLatched = false;
        AppDevice.getBrightness().then((val) {
          if (_pointer == event.pointer) {
            _initialBrightness = _brightness = val;
          }
        });
      } else {
        _swipeAction = SwipeAction.episode;
      }
    }
    _beginHold();
  }

  void pointerMove(PointerMoveEvent event) {
    if (_pointer != event.pointer || _origin == null) return;
    _lastPosition = event.localPosition;
    final diff = event.localPosition - _origin!;
    // 用 _swipeThreshold（屏高 8%，30~80px）而不是固定 12px：
    // 长按倍速时手指几乎不可能完全静止，12px 的抖动就会 _endHold 取消倍速，
    // 表现为「长按没反应」。真正开始滑动（超过阈值）再取消 boost 才符合预期。
    if (diff.distance > _swipeThreshold) {
      _moved = true;
      _endHold();
    }
    if (!_swipeEnabled || !_moved || _viewHeight <= 0 || _viewWidth <= 0)
      return;

    if (_swipeAction == SwipeAction.brightness) {
      // 亮度的 latch 条件：垂直位移超过阈值，且明显大于水平位移。
      // 未 latch 前（长按不动 / 双击 / 斜向微动）不改变亮度也不显示 HUD。
      if (!_brightnessLatched) {
        // 左半屏横向拖动：改判为进度调整。加进度入口后左半屏同时承载
        // 「竖向调亮度 / 横向调进度」，与右半屏的进度手势保持一致。
        final horizontal =
            diff.dx.abs() > _swipeThreshold && diff.dx.abs() > diff.dy.abs();
        if (horizontal) {
          _swipeAction = SwipeAction.progress;
          _seekSeconds = _progressSeconds(diff.dx);
          final preview = _seekHint(_seekSeconds);
          if (preview.isNotEmpty) {
            _progressPreviewing = true;
            hint(preview, persistent: true);
          }
          return;
        }
        final vertical =
            diff.dy.abs() > _swipeThreshold && diff.dy.abs() > diff.dx.abs();
        if (!vertical) return;
        _brightnessLatched = true;
        // 以 latch 时刻的位置为基准，避免从阈值点开始出现亮度跳变。
        _brightnessAnchorDy = event.localPosition.dy;
        _showHud(SwipeAction.brightness, _brightness);
        return;
      }
      final ratio =
          (_brightnessAnchorDy - event.localPosition.dy) / (_viewHeight * 0.6);
      _brightness = (_initialBrightness + ratio).clamp(0.01, 1.0);
      AppDevice.setBrightness(_brightness);
      _showHud(SwipeAction.brightness, _brightness);
    } else if (_swipeAction == SwipeAction.episode ||
        _swipeAction == SwipeAction.progress) {
      // 右半屏：水平主方向一旦超过阈值即 latch 为进度调整（不可逆），
      // 垂直主方向维持切集语义，pointerUp 按最终 latch 结果分发。
      final dx = diff.dx;
      if (_swipeAction == SwipeAction.episode &&
          dx.abs() > _swipeThreshold &&
          dx.abs() > diff.dy.abs()) {
        _swipeAction = SwipeAction.progress;
      }
      if (_swipeAction == SwipeAction.progress) {
        _seekSeconds = _progressSeconds(dx);
        final preview = _seekHint(_seekSeconds);
        if (preview.isNotEmpty) {
          // 拖动预览属进程态提示，抑制 chrome（+90，同倍速 boost）。
          _progressPreviewing = true;
          hint(preview, persistent: true);
        }
      }
    }
  }

  void pointerUp(PointerUpEvent event) {
    _pointers.remove(event.pointer);
    if (_cancelUntilRelease) {
      _ignoreTapUntil = DateTime.now().add(const Duration(milliseconds: 600));
      if (_pointers.isEmpty) _cancelUntilRelease = false;
      return;
    }
    if (_pointer != event.pointer || _origin == null) return;
    final delta = (_lastPosition ?? event.localPosition) - _origin!;
    final isVertical =
        delta.dy.abs() >= _swipeThreshold &&
        delta.dy.abs() > delta.dx.abs() * 1.5;

    if (_swipeAction == SwipeAction.progress && _swipeEnabled && _moved) {
      // 横向拖动提交（左右半屏一致）：seek() 自带 available/duration 校验与
      // clamp，并发出与双击快进一致的最终提示（自动清除拖动期的常驻预览）。
      // 提交即结束进程态，解除 chrome 抑制，最终提示按普通提示处理。
      _progressPreviewing = false;
      if (_seekSeconds != 0) seek(_seekSeconds);
    } else if (_swipeAction == SwipeAction.episode &&
        _swipeEnabled &&
        !_held &&
        _moved &&
        isVertical &&
        event.timeStamp - _started < const Duration(milliseconds: 1500)) {
      if (available()) hint(onEpisode(delta.dy < 0 ? 1 : -1));
    } else if (_swipeAction == SwipeAction.brightness && _brightnessLatched) {
      _scheduleDismissHud();
    }

    if (_moved || _held) {
      _ignoreTapUntil = DateTime.now().add(const Duration(milliseconds: 600));
    }
    _pointer = null;
    _origin = null;
    _endHold();
  }

  void pointerCancel(PointerCancelEvent event) {
    _pointers.remove(event.pointer);
    cancel();
    _ignoreTapUntil = DateTime.now().add(const Duration(milliseconds: 600));
    if (_pointers.isEmpty) _cancelUntilRelease = false;
  }

  void _showHud(SwipeAction action, double value) {
    if (_disposed) return;
    _hudTimer?.cancel();
    _hudState = GestureHudState(type: action, value: value);
    notifyListeners();
  }

  void _scheduleDismissHud() {
    _hudTimer?.cancel();
    _hudTimer = Timer(const Duration(milliseconds: 1000), () {
      if (_disposed) return;
      _hudState = const GestureHudState();
      notifyListeners();
    });
  }

  void seek(int seconds) {
    if (!available() || player.state.duration <= Duration.zero) return;
    _endHold();
    final target = (player.state.position.inMilliseconds + seconds * 1000)
        .clamp(0, player.state.duration.inMilliseconds);
    unawaited((onSeek ?? player.seek)(Duration(milliseconds: target)));
    hint('${seconds > 0 ? '快进至' : '后退至'} ${formatPosition(target / 1000)}');
  }

  void changeVolume(double delta) {
    if (!available()) return;
    final volume = (player.state.volume + delta).clamp(0.0, 100.0);
    unawaited(player.setVolume(volume));
    if (volume > 0) _unmutedVolume = volume;
    hint(volume == 0 ? '已静音' : '音量 ${volume.round()}%');
  }

  void toggleMute() {
    if (!available()) return;
    final current = player.state.volume;
    if (current > 0) _unmutedVolume = current;
    final target = current > 0 ? 0.0 : _unmutedVolume;
    unawaited(player.setVolume(target));
    hint(target == 0 ? '已静音' : '音量 ${target.round()}%');
  }

  KeyEventResult key(KeyEvent event) {
    final key = event.logicalKey;
    if (event is KeyUpEvent) {
      if (key == LogicalKeyboardKey.arrowRight && _keyboardHold) {
        _endHold(tap: available());
        return KeyEventResult.handled;
      }
      return KeyEventResult.ignored;
    }
    final hardware = HardwareKeyboard.instance;
    if (hardware.isAltPressed ||
        hardware.isMetaPressed ||
        hardware.isShiftPressed) {
      cancel();
      return KeyEventResult.ignored;
    }
    if (key == LogicalKeyboardKey.f11 || key == LogicalKeyboardKey.keyF) {
      if (event is KeyDownEvent) {
        cancel();
        onFullscreen();
      }
      return KeyEventResult.handled;
    }
    if (hardware.isControlPressed || !available()) {
      return KeyEventResult.ignored;
    }
    if (key != LogicalKeyboardKey.arrowRight) _endHold();
    if (key == LogicalKeyboardKey.arrowRight) {
      if (event is KeyDownEvent) _beginHold(keyboard: true);
    } else if (key == LogicalKeyboardKey.arrowLeft) {
      seek(-5);
    } else if (key == LogicalKeyboardKey.arrowUp) {
      changeVolume(5);
    } else if (key == LogicalKeyboardKey.arrowDown) {
      changeVolume(-5);
    } else if (key == LogicalKeyboardKey.space ||
        key == LogicalKeyboardKey.mediaPlayPause) {
      if (event is KeyDownEvent) onTogglePlayback();
    } else if (key == LogicalKeyboardKey.keyM) {
      if (event is KeyDownEvent) toggleMute();
    } else if (key == LogicalKeyboardKey.mediaTrackNext ||
        key == LogicalKeyboardKey.mediaTrackPrevious) {
      if (event is KeyDownEvent) {
        hint(onEpisode(key == LogicalKeyboardKey.mediaTrackNext ? 1 : -1));
      }
    } else {
      return KeyEventResult.ignored;
    }
    return KeyEventResult.handled;
  }

  @override
  void dispose() {
    _disposed = true;
    _holdTimer?.cancel();
    _hintTimer?.cancel();
    _hudTimer?.cancel();
    AppDevice.resetBrightness(); // 离开播放器时自动恢复手机/平板系统默认亮度
    if (_boosting) unawaited(_setRate(baseSpeed()));
    _boosting = false;
    _playing.cancel();
    _pointers.clear();
    super.dispose();
  }
}
