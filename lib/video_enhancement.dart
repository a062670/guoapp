import 'dart:async';
import 'dart:io';
import 'dart:math' as math;
import 'dart:ui';

import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:media_kit/media_kit.dart';
import 'package:media_kit_video/media_kit_video.dart';

import 'video_enhancement_assets.dart';
import 'video_enhancement_mpv.dart';
import 'video_enhancement_policy.dart';
import 'video_enhancement_preferences.dart';
import 'video_output_size.dart';

class VideoEnhancementController extends ChangeNotifier {
  VideoEnhancementController({
    required this.player,
    required VideoController? video,
    required VideoEnhancementPreferences preferences,
    required String category,
    required List<String> tags,
    this.fixedSurface,
  }) : _preferences = preferences,
       _animationHint = videoHasAnimationTags(category, tags) {
    final native = player.platform;
    // [移植改造] 原始实现为：
    //     supported = video != null && native is NativePlayer && Platform.isWindows;
    // 即画质增强（含 Anime4K）仅对 Windows 放行。
    //
    // Android 自 PR#2（Android 播放回归 media_kit/libmpv）起已满足条件：
    //   1. player_screen.dart 移除了 `Platform.isAndroid ? LunaExoPlayer() : Player(...)` 分支，
    //      全平台统一使用 media_kit 的 Player，因此 native 在 Android 上同样是 NativePlayer（libmpv）；
    //   2. `_video = widget.videoBuilder == null ? VideoController(...) : null`，
    //      去掉了 `&& !Platform.isAndroid`，Android 也会创建 VideoController，
    //      画面走 `Video(controller: _video!)`，进入 mpv 自己的 GL 渲染管线，
    //      `glsl-shaders` 注入点随之成立。
    //
    // 因此这里去掉 Platform.isWindows 限制。风险控制不在此处做，而是交给
    // video_enhancement_policy.dart 的分层降级：GLES >= 3.0 探测、输入分辨率上限、
    // 倍率封顶、显存预算、温控/省电/倍速自动回落，任一不满足都会自动回退到原画或高质量缩放。
    supported = video != null && native is NativePlayer;
    if (!supported) {
      _status = '当前平台暂不支持画质增强';
      return;
    }
    _mpv = VideoEnhancementMpv(native as NativePlayer);
    _output = VideoOutputSizeAdapter(video!, onFailure: _outputFailed);
    _subscriptions.add(player.stream.log.listen(_onLog));
    _subscriptions.add(player.stream.videoParams.listen((_) => _schedule()));
    _subscriptions.add(
      player.stream.rate.listen((_) {
        _ignorePerformanceUntil = DateTime.now().add(
          const Duration(seconds: 5),
        );
        _schedule();
      }),
    );
    _subscriptions.add(
      player.stream.buffering.listen((buffering) {
        if (buffering) ignorePerformance();
      }),
    );
    _initialization = _initialize();
    _timer = Timer.periodic(const Duration(seconds: 2), (_) => _startMonitor());
  }

  static const _device = MethodChannel('duanju/device');
  static const _filterLabel = 'duanju_vsr';
  final Player player;

  /// [guoguo fork] 固定输出表面策略（安卓端真超分的开关，null = 上游行为）。
  ///
  /// 由 player_screen 在满足全部条件时下发（见 PlayerScreen._fixedSurfacePolicy）。
  /// fork（packages/media_kit_video）会在**首次** videoParams 事件上按该策略把
  /// 渲染表面一次性放大到「源尺寸 × 倍率」并锁定，本类据此读取实际表面尺寸
  /// （见 [_fixedSurfaceSize]）参与决策——表面对不上时 _monitor 的 sizeMatches
  /// 会连续不成立，5 次即 _failBackend 把后端拉黑。
  final VideoFixedSurface? fixedSurface;

  /// 固定表面模式下 fork **实际应用**的表面尺寸（未启用/尚未应用时为 null）。
  ///
  /// 为什么不自己按公式算：片源分辨率变化（切清晰度、自适应码率）后，公式
  /// 算出的期望尺寸会与 fork 已锁定的表面对不上，sizeMatches 连续不成立会把
  /// 后端误拉黑。rect 是 fork 应用表面后回传的唯一事实来源，锁定后永不变化，
  /// 天然与 sizeMatches 一致；表面对不上当前源时（新源更大）expectsUpscale 也
  /// 会正确地不为真，与 mpv 跳过 Upscale 的实际行为对齐。
  Size? get _fixedSurfaceSize =>
      fixedSurface == null ? null : _output?.actualSize;

  late final bool supported;
  final bool _animationHint;
  VideoEnhancementPreferences _preferences;
  VideoEnhancementMpv? _mpv;
  VideoOutputSizeAdapter? _output;
  Future<void>? _initialization;
  Future<void>? _monitoring;
  Future<void>? _closing;
  Future<void> _tail = Future<void>.value();
  final _subscriptions = <StreamSubscription<dynamic>>[];
  final _ownedShaders = <String>{};
  final _originalOptions = <String, String>{};
  final _appliedOptions = <String, String>{};
  final _rejected = <VideoEnhancementBackend>{};
  final _renderSamples = <double>[];
  final _recentEnhancementErrors = <String>{};
  Timer? _timer;
  Timer? _debounce;
  int _revision = 0;
  int _costLimit = 4;
  int _confirmationMisses = 0;
  int _failureCount = 0;
  int? _previousDrops;
  DateTime? _previousSample;
  DateTime? _overloadedSince;
  DateTime _ignorePerformanceUntil = DateTime.fromMillisecondsSinceEpoch(0);
  VideoEnhancementPower _power = const VideoEnhancementPower();
  VideoEnhancementDecision? _applied;
  /// [_appliedTier·2026-10] _applied 生效时所用的 Anime4K 档位。
  /// decision 不含 tier（档位只换链、不换后端/尺寸），播放中切档位时
  /// `selected == _applied` 仍成立，需靠本字段识别「同后端不同链」
  /// 从而触发重注；非 anime 后端无所谓档位。
  VideoEnhancementTier _appliedTier = VideoEnhancementTier.balanced;

  /// [阶梯降档·2026-10 +142] 本播放会话的档位覆盖，仅由 _watchPerformance
  /// 过载降档写入，null 表示按用户偏好。刻意不做持久化：降档是「这一集跑
  /// 不动」的临时让让，不该悄悄改掉用户保存的偏好；用户在设置页动过任何
  /// 选项（setPreferences）或换集（beforeMedia）即清除，回到用户的选择。
  VideoEnhancementTier? _tierOverride;

  /// 实际应注入的档位：会话级降档覆盖优先，否则用户偏好。
  VideoEnhancementTier get _effectiveTier => _tierOverride ?? _preferences.tier;
  VideoEnhancementBackend? _applyingBackend;
  bool _ready = false;
  bool _initialized = false;
  bool _foreground = true;
  bool _television = false;
  bool _closed = false;
  bool _comparing = false;
  bool _confirmed = false;
  bool _gpu = false;
  bool _luma = false;
  bool _opaque8Bit = false;
  bool _hardware = false;
  bool _softwareRenderer = false;
  bool _circuitOpen = false;
  String _renderer = '';
  String _vendor = '';
  String _status = '画质增强已关闭';
  String _downgradeReason = '';
  String _lastFailure = '';
  // [诊断·2026-10] 最近一次增强失败的细节，经 detail 展示到设置面板。
  // 安卓端真机实测 anime 应用后确认失败（「增强不可用，已自动回退」），
  // 但失败可能来自三个不同环节（mpv 报错 / 渲染链未带上了 shader /
  // 表面尺寸不匹配），修法完全不同，故先采集再修：
  //   - _lastFailureDetail：阶段 + 尺寸比对 + 渲染链 pass 描述 + 最近 mpv 日志
  //   - _monitorSnapshot：最近一次确认未通过时的 _monitor 现场快照
  String _lastFailureDetail = '';
  String _monitorSnapshot = '';
  Size _viewport = Size.zero;

  VideoEnhancementPreferences get preferences => _preferences;
  bool get comparing => _comparing;
  bool get canCompare =>
      supported &&
      _ready &&
      _foreground &&
      !_circuitOpen &&
      _preferences.mode != VideoEnhancementMode.off &&
      (_comparing || (_applied?.backend.cost ?? 0) > 0);
  bool get animation => switch (_preferences.content) {
    VideoEnhancementContent.automatic => _animationHint,
    VideoEnhancementContent.animation => true,
    VideoEnhancementContent.general => false,
  };
  String get status => _status;
  String get contentLabel => animation ? '当前按动漫增强' : '当前按通用画面增强';
  String get detail {
    // [文案·2026-10] 安卓端默认表面恒为源尺寸（+82 决策），启用固定表面后
    // 表面可大于源，但「源 × → 表面 ×」的尺寸对比 + 「超分在渲染链内部完成」
    // 的说明叠加仍容易被误读为未生效。按产品要求：安卓端神经增强（cost≥3）
    // 只呈现一句结果文案，不再展示尺寸对比与内部渲染说明；其余平台/后端
    // （桌面端放大、ravu 缩放等）维持原 dimensions 展示。
    if (Platform.isAndroid && (_applied?.backend.cost ?? 0) >= 3) {
      return [
        // 三档在安卓端默认都是同分辨率重建/降噪；固定大表面启用后 Upscale
        // 链参与渲染，但文案按 +88 的产品要求仍统一为这一句。
        '已执行超分降噪',
        if (_downgradeReason.isNotEmpty) _downgradeReason,
        // [诊断·2026-10] 最近一次增强失败的技术细节（阶段/尺寸比对/渲染链
        // pass 描述/mpv 日志）。空串时自动过滤，不影响正常态排版。
        if (_lastFailureDetail.isNotEmpty) _lastFailureDetail,
      ].where((item) => item.isNotEmpty).join(' · ');
    }
    final source = videoDisplaySize(player.state.videoParams);
    final output = _applied?.output;
    final dimensions = source != null && output != null
        ? '${source.width.round()}×${source.height.round()} → ${output.width.round()}×${output.height.round()}'
        : '';
    return [
      dimensions,
      if (_downgradeReason.isNotEmpty) _downgradeReason,
      if (_lastFailureDetail.isNotEmpty) _lastFailureDetail,
    ].where((item) => item.isNotEmpty).join(' · ');
  }

  Future<void> _initialize() async {
    try {
      await _mpv!.initialize();
      if (_closed) return;
      await _output!.initialize();
      if (_closed) return;
      _initialized = true;
      await _readPower();
      _readCapabilities();
      _schedule();
    } catch (_) {
      if (!_closed) _setStatus('增强初始化失败，继续原画播放');
    }
  }

  void setPreferences(VideoEnhancementPreferences value) {
    if (_closed || value == _preferences) return;
    _preferences = value;
    _comparing = false;
    _costLimit = 4;
    // 用户主动改设置 = 新的一次尝试：本集的自动降档覆盖作废，回到用户所选。
    _tierOverride = null;
    _rejected.clear();
    _downgradeReason = '';
    _lastFailure = '';
    // [诊断·2026-10] 用户主动改设置 = 新的一次尝试，旧失败现场全部作废。
    _lastFailureDetail = '';
    _monitorSnapshot = '';
    _failureCount = 0;
    _circuitOpen = false;
    _schedule(immediate: true);
    notifyListeners();
  }

  void setViewport(Size pixels, {required bool television}) {
    if (_closed || !supported) return;
    if (_viewport == pixels && _television == television) return;
    _viewport = pixels;
    _television = television;
    ignorePerformance();
    _schedule();
  }

  void setForeground(bool value) {
    if (_closed || _foreground == value) return;
    _foreground = value;
    ignorePerformance();
    _schedule(immediate: true);
  }

  void suspend() {
    if (_closed) return;
    _ready = false;
    _comparing = false;
    _revision++;
    _schedule(immediate: true);
  }

  Future<void> beforeMedia() async {
    if (_closed || !supported) return;
    _ready = false;
    _comparing = false;
    _revision++;
    _debounce?.cancel();
    _costLimit = 4;
    // 换集 = 全新会话，上一集的阶梯降档不带过来。
    _tierOverride = null;
    _downgradeReason = '';
    _lastFailure = '';
    // [诊断·2026-10] 换集 = 全新播放会话，上一集的失败现场不带过来。
    _lastFailureDetail = '';
    _monitorSnapshot = '';
    _previousDrops = null;
    _previousSample = null;
    _renderSamples.clear();
    _recentEnhancementErrors.clear();
    _failureCount = 0;
    _circuitOpen = false;
    ignorePerformance();
    await _enqueue(() async {
      await _initialization;
      if (_closed || !_initialized) return;
      _clearEffects();
      await _output!.setTarget(null);
      _applied = null;
      _confirmed = false;
    });
  }

  void mediaReady() {
    if (_closed) return;
    _ready = true;
    ignorePerformance();
    _schedule();
  }

  void ignorePerformance() {
    _ignorePerformanceUntil = DateTime.now().add(const Duration(seconds: 6));
    _overloadedSince = null;
    _renderSamples.clear();
    _previousDrops = null;
    _previousSample = null;
  }

  Future<void> toggleCompare() async {
    if (!canCompare || _closed) return;
    _comparing = !_comparing;
    ignorePerformance();
    _schedule(immediate: true);
    notifyListeners();
    await _tail;
  }

  void _schedule({bool immediate = false}) {
    if (_closed || !supported || _circuitOpen) return;
    final revision = ++_revision;
    _debounce?.cancel();
    if (immediate) {
      unawaited(_enqueue(() => _apply(revision)));
    } else {
      _debounce = Timer(const Duration(milliseconds: 220), () {
        unawaited(_enqueue(() => _apply(revision)));
      });
    }
  }

  Future<void> _enqueue(Future<void> Function() work) {
    final next = _tail.then((_) async {
      if (_closed) return;
      try {
        await work();
      } catch (error) {
        if (!_closed) _failBackend(error);
      }
    });
    _tail = next.catchError((Object _) {});
    return _tail;
  }

  Future<void> _apply(int revision) async {
    await _initialization;
    if (_closed || !_initialized || revision != _revision) return;
    _readCapabilities();
    final decision = chooseVideoEnhancement(
      preferences: _preferences,
      parameters: player.state.videoParams,
      viewport: _viewport,
      ready: _ready && _foreground,
      android: Platform.isAndroid,
      television: _television,
      gpu: _gpu,
      luma: _luma,
      hardware: _hardware,
      animation: animation,
      power: _power,
      rate: player.state.rate,
      costLimit: _costLimit,
      rejected: _rejected,
      fixedSurface: _fixedSurfaceSize,
      opaque8Bit: _opaque8Bit,
    );
    final selected = _comparing && canCompare
        ? VideoEnhancementDecision(
            VideoEnhancementBackend.original,
            decision.output,
            '原画对比中',
          )
        : decision;
    // [档位·2026-10] anime 后端播放中切 tier 时 decision 不变但链不同，
    // 必须重注；其他后端切 tier 无意义，保持提前返回避免无谓重注入。
    final tierMatters = selected.backend == VideoEnhancementBackend.anime;
    if (selected == _applied &&
        (!tierMatters || _appliedTier == _effectiveTier)) {
      _publishStatus();
      return;
    }
    _applyingBackend = selected.backend;
    try {
      final shaders = switch (selected.backend) {
        VideoEnhancementBackend.ravu => <String>[
          _luma
              ? VideoEnhancementAssets.ravuLuma
              : VideoEnhancementAssets.ravuRgb,
        ],
        VideoEnhancementBackend.fsrcnnx => <String>[
          VideoEnhancementAssets.fsrcnnx,
        ],
        // [档位·2026-10] 按 tier 选链：质量档=VL 链，精细档=M/S 链
        // （+87 引入；+144 起默认档为均衡档）。两链 WHEN 均保留，平台筛选由 mpv 完成。
        // [阶梯降档·+142] 用 _effectiveTier 而非 _preferences.tier：看门狗
        // 降档只改会话级覆盖，用户保存的偏好不变（见 _tierOverride 注释）。
        VideoEnhancementBackend.anime => switch (_effectiveTier) {
          VideoEnhancementTier.quality =>
            VideoEnhancementAssets.animeChainQuality,
          VideoEnhancementTier.fine => VideoEnhancementAssets.animeChainFine,
          VideoEnhancementTier.standard =>
            VideoEnhancementAssets.animeChainStandard,
          VideoEnhancementTier.balanced =>
            VideoEnhancementAssets.animeChainBalanced,
          VideoEnhancementTier.minimalUpscale =>
            VideoEnhancementAssets.animeChainMinimalUpscale,
          VideoEnhancementTier.minimal =>
            VideoEnhancementAssets.animeChainMinimal,
        },
        _ => null,
      };
      final shaderPaths = shaders == null
          ? null
          : await Future.wait(shaders.map(VideoEnhancementAssets.prepare));
      if (_closed || revision != _revision) return;
      _applied = null;
      _clearEffects();
      await _output!.setTarget(selected.output);
      if (_closed || revision != _revision) return;
      if (selected.backend != VideoEnhancementBackend.original) {
        _setOption('scale', 'spline36');
        _setOption('dscale', 'mitchell');
        _setOption('cscale', 'bilinear');
        _setOption('correct-downscaling', 'yes');
        if (shaderPaths != null) {
          _setOption('fbo-format', 'rgba16f');
          final current = _shaderList();
          _ownedShaders.addAll(shaderPaths);
          _mpv!.setStrings('glsl-shaders', [...current, ...shaderPaths]);
        } else if (selected.backend == VideoEnhancementBackend.hardware) {
          final parameters = player.state.videoParams;
          final factor = math
              .min(
                selected.output!.width / parameters.w!,
                selected.output!.height / parameters.h!,
              )
              .clamp(1.0, 2.0);
          _mpv!.command([
            'vf',
            'add',
            '@$_filterLabel:d3d11vpp=scale=${factor.toStringAsFixed(6)}:scaling-mode=$_vendor:format=nv12:deint=no',
          ]);
        }
      }
      _applied = selected;
      _appliedTier = _effectiveTier;
      _confirmationMisses = 0;
      _confirmed = selected.backend == VideoEnhancementBackend.original;
      ignorePerformance();
      _publishStatus();
      if (!_closed) notifyListeners();
    } catch (error) {
      _failBackend(error, backend: selected.backend);
    } finally {
      _applyingBackend = null;
    }
  }

  String _optionValue(Object? value) => switch (value) {
    bool flag => flag ? 'yes' : 'no',
    null => '',
    _ => value.toString(),
  };

  void _setOption(String key, String value) {
    final original = _mpv!.read(key);
    if (original == null) throw VideoEnhancementFailure(key);
    _originalOptions.putIfAbsent(key, () => _optionValue(original));
    _mpv!.set(key, value);
    _appliedOptions[key] = value;
  }

  List<String> _shaderList() {
    final value = _mpv!.read('glsl-shaders');
    if (value is! List) throw const VideoEnhancementFailure('shader-list');
    return value.whereType<String>().toList();
  }

  void _clearEffects() {
    if (_mpv == null || !_initialized) return;
    Object? failure;
    try {
      if (_ownedShaders.isNotEmpty) {
        final current = _shaderList();
        final remaining = current
            .where((item) => !_ownedShaders.contains(item))
            .toList();
        if (current.length != remaining.length)
          _mpv!.setStrings('glsl-shaders', remaining);
      }
    } catch (error) {
      failure = error;
    }
    try {
      final filters = _mpv!.read('vf');
      if (filters is List &&
          filters.any((item) => item is Map && item['label'] == _filterLabel)) {
        _mpv!.command(['vf', 'remove', '@$_filterLabel']);
      }
    } catch (error) {
      failure ??= error;
    }
    for (final entry in _appliedOptions.entries.toList().reversed) {
      try {
        if (_optionValue(_mpv!.read(entry.key)) == entry.value) {
          _mpv!.set(entry.key, _originalOptions[entry.key]!);
        }
        _appliedOptions.remove(entry.key);
      } catch (error) {
        failure ??= error;
      }
    }
    if (failure != null) throw failure;
  }

  void _readCapabilities() {
    if (_closed || !_initialized) return;
    final native = _mpv!;
    final vo = native.read('current-vo')?.toString() ?? '';
    final hwdec = native.read('hwdec-current')?.toString() ?? '';
    final version = native.read('mpv-version')?.toString() ?? '';
    final versionNumber = RegExp(r'\bv?0\.(\d+)\.').firstMatch(version);
    final recentMpv =
        versionNumber != null && (int.tryParse(versionNumber[1]!) ?? 0) >= 39;
    final passes = native.read('vo-passes');
    _gpu =
        !_softwareRenderer &&
        (vo == 'gpu' ||
            vo == 'libmpv' && (_renderer.isNotEmpty || passes is Map));
    final parameters = player.state.videoParams;
    final format = videoEnhancementPixelFormat(parameters);
    _luma =
        !(Platform.isAndroid && hwdec == 'mediacodec') &&
        (format.startsWith('yuv') || format == 'nv12' || format == 'nv21');
    _opaque8Bit = false;
    if (Platform.isAndroid && hwdec == 'mediacodec' && format == 'mediacodec') {
      final tracks = native.read('track-list');
      if (tracks is List) {
        for (final track in tracks.whereType<Map>()) {
          if (track['type'] != 'video' || track['selected'] != true) continue;
          final codec = track['codec']?.toString().toLowerCase() ?? '';
          final profile =
              track['codec-profile']?.toString().toLowerCase() ?? '';
          _opaque8Bit =
              codec == 'h264' &&
                  {
                    'baseline',
                    'constrained baseline',
                    'main',
                    'extended',
                    'high',
                    'constrained high',
                  }.contains(profile) ||
              codec == 'hevc' &&
                  {'main', 'main still picture'}.contains(profile);
          break;
        }
      }
    }
    _hardware =
        Platform.isWindows &&
        _gpu &&
        recentMpv &&
        (_vendor == 'nvidia' || _vendor == 'intel') &&
        hwdec == 'd3d11va' &&
        (format == 'nv12' || format == 'yuv420p');
  }

  void _onLog(PlayerLog log) {
    if (_closed) return;
    final prefix = log.prefix.toLowerCase();
    final text = log.text.toLowerCase();
    if ((prefix.contains('vo/') || prefix.contains('libmpv')) &&
        (text.contains('gl_renderer') ||
            text.contains('gl_vendor') ||
            text.contains('gl renderer') ||
            text.contains('gl vendor'))) {
      if (text.contains('renderer')) {
        _renderer = log.text.length <= 512
            ? log.text
            : log.text.substring(0, 512);
        _vendor = text.contains('nvidia')
            ? 'nvidia'
            : text.contains('intel')
            ? 'intel'
            : '';
      }
      _softwareRenderer =
          _softwareRenderer ||
          RegExp(
            r'swiftshader|llvmpipe|softpipe|software rasterizer|microsoft basic',
          ).hasMatch(text);
      _schedule();
    }
    final active = _applyingBackend ?? _applied?.backend;
    if (active == null ||
        active == VideoEnhancementBackend.original ||
        _circuitOpen)
      return;
    final graphics =
        prefix.contains('d3d11vpp') ||
        prefix.contains(_filterLabel) ||
        prefix.contains('vo/gpu') ||
        prefix.contains('vo/libmpv');
    final failed =
        log.level == 'error' ||
        log.level == 'fatal' ||
        log.level == 'warn' &&
            RegExp(r'fail|error|unsupported|disabled').hasMatch(text);
    if (graphics && failed) {
      if (_recentEnhancementErrors.length >= 16)
        _recentEnhancementErrors.clear();
      _recentEnhancementErrors.add(log.text.trim());
      _failBackend(const VideoEnhancementFailure('render'));
    }
  }

  bool handlePlaybackError(String text) {
    if (_closed || !supported) return false;
    final value = text.trim();
    final active = (_applyingBackend ?? _applied?.backend)?.cost ?? 0;
    final owned =
        _recentEnhancementErrors.contains(value) ||
        active > 0 &&
            RegExp(
              r'd3d11vpp|duanju_vsr|glsl.shaders|shader.*compil|compil.*shader',
              caseSensitive: false,
            ).hasMatch(value);
    if (!owned) return false;
    _failBackend(const VideoEnhancementFailure('render'));
    return true;
  }

  void _outputFailed() {
    if (_closed || _costLimit == 0) return;
    _costLimit = 0;
    _failBackend(const VideoEnhancementFailure('output-size'));
  }

  void _failBackend(Object error, {VideoEnhancementBackend? backend}) {
    if (_closed || _circuitOpen) return;
    final failed = backend ?? _applyingBackend ?? _applied?.backend;
    _failureCount++;
    _applyingBackend = null;
    if (failed != null) _rejected.add(failed);
    if (failed == VideoEnhancementBackend.scaling ||
        failed == VideoEnhancementBackend.original ||
        error is VideoEnhancementFailure && error.stage == 'output-size') {
      _costLimit = 0;
    }
    _lastFailure = error is VideoEnhancementFailure ? error.stage : 'resource';
    _downgradeReason = '增强不可用，已自动回退';
    // [诊断·2026-10] 汇总「阶段 + 确认现场 + 最近渲染日志」三段，经 detail
    // 展示到设置面板。阶段告诉你从哪条路径失败的；现场快照区分尺寸/渲染链
    // 两种根因；日志兜住 shader 编译失败等 render 路径（mpv 的报错只在
    // _onLog 里闪现，不落盘，必须在此刻抓取）。
    final logs = _recentEnhancementErrors
        .take(2)
        .map((item) => item.length <= 80 ? item : '${item.substring(0, 80)}…')
        .join(' | ');
    final assembled = <String>[
      '阶段:$_lastFailure',
      if (_monitorSnapshot.isNotEmpty) _monitorSnapshot,
      if (logs.isNotEmpty) '日志:$logs',
    ].join(' ');
    _lastFailureDetail = assembled.length <= 360
        ? assembled
        : '${assembled.substring(0, 360)}…';
    _applied = null;
    _confirmed = false;
    _setStatus('正在恢复原画播放');
    if (_failureCount >= 4 || failed == VideoEnhancementBackend.original) {
      _circuitOpen = true;
      _costLimit = 0;
      _revision++;
      _debounce?.cancel();
      unawaited(
        _enqueue(() async {
          try {
            _clearEffects();
            await _output?.setTarget(null);
            if (_closed) return;
            _applied = const VideoEnhancementDecision(
              VideoEnhancementBackend.original,
              null,
              '增强不可用，已恢复原画',
            );
            _setStatus('增强不可用，已恢复原画');
          } catch (_) {
            _setStatus('增强已停用，请重新打开本集');
          }
        }),
      );
      return;
    }
    _schedule(immediate: true);
  }

  // [诊断·2026-10] 把一次「确认未通过」的现场整理成可读文本，供 detail 展示。
  //
  // 安卓端真机实测：高级档 anime 应用后走到「正在确认」却始终不 confirmed，
  // 连续 5 次 miss 后回退。失败只可能来自三个环节，修法完全不同：
  //   1. 尺寸不匹配：rect / osd-dimensions 与 decision.output 对不上
  //      （安卓 surfaceOutput=源尺寸，rect 由 media_kit 维护，理论上应一致，
  //      若不一致说明 media_kit 的表面同步链有意外行为）；
  //   2. 渲染链未带 shader：vo-passes 的 fresh 描述里找不到 Anime4K /
  //      Depth-to-Space（libmpv VO 下 vo-passes 的报告口径可能与桌面端
  //      gpu VO 不同，或 shader 实际没进渲染链）；
  //   3. 两者都过但仍失败（理论不存在，会归到 render-pass）。
  // 不分辨根因就动手修，大概率修错方向，故先采集。
  String _describeConfirmation({
    required Size? actual,
    required Object? surfaceWidth,
    required Object? surfaceHeight,
    required Size? expected,
    required bool sizeMatches,
    required bool effectFound,
    required String descriptions,
  }) {
    String formatNum(Object? value) => value is num ? '${value.round()}' : '无';
    String formatBox(Size? size) =>
        size == null ? '无' : '${size.width.round()}×${size.height.round()}';
    final buffer = StringBuffer(
      '尺寸${sizeMatches ? '匹配' : '不匹配'}'
      '/渲染链${effectFound ? '已带shader' : '未带shader'}',
    );
    buffer.write(' 期望${formatBox(expected)}');
    buffer.write(' 表面${formatBox(actual)}');
    buffer.write(' osd${formatNum(surfaceWidth)}×${formatNum(surfaceHeight)}');
    final lines = descriptions
        .split('\n')
        .map((line) => line.trim())
        .where((line) => line.isNotEmpty)
        .take(8)
        .map((line) => line.length <= 60 ? line : '${line.substring(0, 60)}…')
        .toList();
    buffer.write(' passes:${lines.isEmpty ? '空' : lines.join(' | ')}');
    final text = buffer.toString();
    return text.length <= 360 ? text : '${text.substring(0, 360)}…';
  }

  void _startMonitor() {
    if (_closed ||
        !_initialized ||
        _monitoring != null ||
        _circuitOpen ||
        !_foreground ||
        !_ready ||
        _preferences.mode == VideoEnhancementMode.off)
      return;
    final future = _monitor();
    _monitoring = future;
    unawaited(
      future.whenComplete(() {
        if (identical(_monitoring, future)) _monitoring = null;
      }),
    );
  }

  Future<void> _readPower() async {
    try {
      final value = await _device
          .invokeMapMethod<String, dynamic>('playbackPower')
          .timeout(const Duration(seconds: 2));
      if (!_closed && value != null)
        _power = VideoEnhancementPower.fromMap(value);
    } catch (_) {}
  }

  Future<void> _monitor() async {
    try {
      await _readPower();
      if (_closed || !_initialized || _circuitOpen || _applyingBackend != null)
        return;
      final oldCapabilities = (_gpu, _luma, _hardware, _opaque8Bit);
      _readCapabilities();
      if (oldCapabilities != (_gpu, _luma, _hardware, _opaque8Bit)) _schedule();
      if (!_ready || !_foreground) return;
      final selected = _applied;
      if (selected == null) {
        _schedule();
        return;
      }
      // 固定表面模式下，策略层认定的 output 必须与 fork 实际应用的表面一致，
      // 因此这里与 _apply 共用同一个取值（_fixedSurfaceSize = fork 已应用的
      // 表面尺寸），否则 reassessed 与 selected 会无限互斥、sizeMatches 也会
      // 连续不成立把后端误拉黑。
      final fixed = _fixedSurfaceSize;
      final reassessed = chooseVideoEnhancement(
        preferences: _preferences,
        parameters: player.state.videoParams,
        viewport: _viewport,
        ready: true,
        android: Platform.isAndroid,
        television: _television,
        gpu: _gpu,
        luma: _luma,
        hardware: _hardware,
        animation: animation,
        power: _power,
        rate: player.state.rate,
        costLimit: _costLimit,
        rejected: _rejected,
        fixedSurface: fixed,
        opaque8Bit: _opaque8Bit,
      );
      if (!_comparing && reassessed != selected) {
        // [移植改造] 此处原先硬编码 `thermalStatus >= 2 || headroom >= .85`，
        // 与 video_enhancement_policy.dart 放宽后的具名常量不一致：真机上
        // thermalStatus=2 很常见（正是当初放宽策略层阈值的原因），导致任何
        // 一次决策变化都会把 _costLimit 永久压低（_costLimit 只在新设置/
        // 下一集/熔断时重置），表现为"进全屏后再也不超分"。
        // 现与策略层共用 kThermalStatusLimit / kThermalHeadroomLimit。
        if (reassessed.backend.cost < selected.backend.cost &&
            (_power.thermalStatus >= kThermalStatusLimit ||
                (_power.headroom ?? 0) >= kThermalHeadroomLimit)) {
          _costLimit = math.min(_costLimit, reassessed.backend.cost);
          _downgradeReason = '本集已因温控降低增强';
        }
        _schedule();
        return;
      }
      if (selected.backend == VideoEnhancementBackend.original) return;
      final passes = _mpv!.read('vo-passes');
      final fresh = passes is Map && passes['fresh'] is List
          ? passes['fresh'] as List
          : const [];
      final descriptions = fresh
          .whereType<Map>()
          .map((item) => item['desc']?.toString() ?? '')
          .join('\n');
      final actual = _output!.actualSize;
      final surface = _mpv!.read('osd-dimensions');
      final surfaceWidth = surface is Map ? surface['w'] : null;
      final surfaceHeight = surface is Map ? surface['h'] : null;
      final sizeMatches =
          actual != null &&
          selected.output != null &&
          (actual.width - selected.output!.width).abs() <= 2 &&
          (actual.height - selected.output!.height).abs() <= 2 &&
          surfaceWidth is num &&
          surfaceHeight is num &&
          (surfaceWidth - selected.output!.width).abs() <= 2 &&
          (surfaceHeight - selected.output!.height).abs() <= 2;
      final sourceSize = videoDisplaySize(player.state.videoParams);
      // 极简档链只有 Clamp + Restore_S，没有 Upscale/Depth-to-Space；固定
      // 大表面下它只是「重建 + spline36 放大到表面」，若仍按含超分链的
      // 档位要求 Depth-to-Space，会必然误报 render-pass（+141 真机实证：
      // 期望/表面/osd 均为 2560×1440、尺寸匹配，但链内无 Depth-to-Space，
      // passes 只有 De-Ring + Restore_S，即极简档）。
      // 用 _appliedTier（实际注入的链）而非 _preferences.tier（可能刚切档
      // 尚未重注），保证期望与链内容一致。
      // （+143 极简超分档带 Upscale_Denoise_S 链，expectsUpscale 为 true，
      // 仍按 Depth-to-Space 判定；六档里只有极简档无超分链。）
      final expectsUpscale =
          fixed != null &&
          _appliedTier != VideoEnhancementTier.minimal &&
          selected.output != null &&
          sourceSize != null &&
          (selected.output!.width > sourceSize.width + 2 ||
              selected.output!.height > sourceSize.height + 2);
      final effectFound = switch (selected.backend) {
        VideoEnhancementBackend.ravu => descriptions.contains(
          _luma ? 'RAVU-Lite-AR (step2' : 'RAVU (step4',
        ),
        VideoEnhancementBackend.anime =>
          // +87 链改造后分平台生效形态不同：桌面端 Upscale pass 生效
          // （末 pass 为 Depth-to-Space），安卓端 Upscale 被 WHEN 跳过、
          // 仅 Clamp + Restore_CNN 生效（pass 描述含 Restore）。二者有其一
          // 即视为链已加载，避免把安卓端的正常 Restore 形态误判为未生效
          // （+95 极简档只有 Clamp + Restore_S，同样命中 Restore）。
          // 固定大表面启用后，输出大于源时必须见到 Depth-to-Space（极简档
          // 无 Upscale 链，expectsUpscale 已置 false，见上），否则说明
          // Upscale 仍被 WHEN 跳过，不能仅凭 Restore 判为成功。
          descriptions.contains('Anime4K') &&
              (expectsUpscale
                  ? descriptions.contains('Depth-to-Space')
                  : (descriptions.contains('Depth-to-Space') ||
                        descriptions.contains('Restore'))),
        VideoEnhancementBackend.fsrcnnx =>
          descriptions.contains('feature map') &&
              descriptions.contains('aggregation'),
        VideoEnhancementBackend.hardware => _hardwareFramesChanged(selected),
        _ => _gpu,
      };
      final wasConfirmed = _confirmed;
      if (sizeMatches && effectFound) {
        _confirmed = true;
        _confirmationMisses = 0;
        // [诊断·2026-10] 确认通过即恢复，此前积压的失败现场作废，
        // 避免把已解决的后端失败细节继续挂在详情里误导排查。
        _monitorSnapshot = '';
        _lastFailureDetail = '';
      } else if (player.state.playing && !player.state.buffering) {
        _confirmed = false;
        _confirmationMisses++;
        if (_confirmationMisses >= 5) {
          // [诊断·2026-10] 确认失败先留现场再回退：尺寸比对与渲染链
          // pass 描述是区分 render-pass / output-size 两种根因的唯一依据，
          // 修法完全不同（前者改 vo-passes 检测，后者查表面尺寸链路）。
          _monitorSnapshot = _describeConfirmation(
            actual: actual,
            surfaceWidth: surfaceWidth,
            surfaceHeight: surfaceHeight,
            expected: selected.output,
            sizeMatches: sizeMatches,
            effectFound: effectFound,
            descriptions: descriptions,
          );
          _failBackend(
            VideoEnhancementFailure(
              sizeMatches ? 'render-pass' : 'output-size',
            ),
          );
          return;
        }
      }
      _publishStatus();
      if (wasConfirmed != _confirmed && !_closed) notifyListeners();
      _watchPerformance(selected, fresh);
    } catch (_) {
      if (!_closed) {
        _previousDrops = null;
        _previousSample = null;
      }
    }
  }

  bool _hardwareFramesChanged(VideoEnhancementDecision selected) {
    final output = _mpv!.read('video-out-params');
    final filters = _mpv!.read('vf');
    if (output is! Map || filters is! List || selected.output == null)
      return false;
    final width = (output['w'] as num?)?.toDouble() ?? 0;
    final height = (output['h'] as num?)?.toDouble() ?? 0;
    return filters.any(
          (item) =>
              item is Map &&
              item['label'] == _filterLabel &&
              item['enabled'] != false,
        ) &&
        (width - selected.output!.width).abs() <= 4 &&
        (height - selected.output!.height).abs() <= 4;
  }

  bool _isOwnPass(
    String description,
    VideoEnhancementBackend backend,
  ) => switch (backend) {
    VideoEnhancementBackend.ravu => description.contains('RAVU'),
    VideoEnhancementBackend.anime => description.contains('Anime4K'),
    VideoEnhancementBackend.fsrcnnx => RegExp(
      r'feature map [12]|mapping [1-4]_[12]|sub-band residuals [12]|sub-pixel convolution 1|aggregation',
    ).hasMatch(description),
    _ => false,
  };

  void _watchPerformance(VideoEnhancementDecision selected, List fresh) {
    final now = DateTime.now();
    final state = player.state;
    if (!_confirmed ||
        !state.playing ||
        state.buffering ||
        _comparing ||
        state.rate > 1.01 ||
        now.isBefore(_ignorePerformanceUntil)) {
      _previousSample = null;
      _previousDrops = null;
      _overloadedSince = null;
      return;
    }
    var fpsValue = _mpv!.read('container-fps');
    if (fpsValue is! num || fpsValue <= 0) {
      fpsValue = _mpv!.read('estimated-vf-fps');
    }
    final fps = fpsValue is num ? fpsValue.toDouble() : 0.0;
    if (!fps.isFinite || fps <= 0 || fps > 240) return;
    var ownMs = 0.0;
    var timed = false;
    for (final pass in fresh.whereType<Map>()) {
      if (!_isOwnPass(pass['desc']?.toString() ?? '', selected.backend))
        continue;
      final last = pass['last'];
      if (last is num && last >= 0) {
        ownMs += last / 1000000;
        timed = true;
      }
    }
    if (timed && ownMs.isFinite) {
      _renderSamples.add(ownMs);
      if (_renderSamples.length > 20) _renderSamples.removeAt(0);
    }
    final samples = [..._renderSamples]..sort();
    final p95 = samples.isEmpty
        ? 0.0
        : samples[((samples.length - 1) * .95).ceil()];
    // [阈值修正·+142] 旧判据是 `p95 > 帧间隔 × 25%` 单独即可触发过载：
    // 骁龙 865 上 7 个 pass 的极简档就会越过 8.3ms（30fps），即便渲染只占
    // 帧间隔 60%、稳跑满帧率也被判超载，10 秒后增强整个消失——实测过
    // 「明明不卡却莫名回退」。改为只看两种真实卡顿信号：
    //   ① 渲染时间已超过整个帧间隔（必然掉帧，只是还没反映到计数）；
    //   ② frame-drop-count 增速 > 1%/秒（mpv 自己报的掉帧）。
    // 「渲染占 30% 但帧率正常」不再触发，交由用户的主观流畅度决定。
    final frameBudgetMs = 1000 / fps / state.rate;
    var overloaded = samples.length >= 5 && p95 > frameBudgetMs;
    final rawDrops = _mpv!.read('frame-drop-count');
    if (rawDrops is num) {
      final drops = rawDrops.toInt();
      final previous = _previousDrops;
      final previousTime = _previousSample;
      if (previous != null && previousTime != null && drops >= previous) {
        final seconds = now.difference(previousTime).inMilliseconds / 1000;
        if (seconds >= 1 && seconds <= 5) {
          overloaded = overloaded || (drops - previous) / (fps * seconds) > .01;
        }
      }
      _previousDrops = drops;
      _previousSample = now;
    }
    if (!overloaded) {
      _overloadedSince = null;
      return;
    }
    _overloadedSince ??= now;
    if (now.difference(_overloadedSince!) < const Duration(seconds: 10)) return;
    // [阶梯降档·+142] 过载确认后先降一档 Anime4K（质量→轻量→标准→均衡→
    // 极简），到极简仍过载才走原来的 costLimit 降级（放弃 anime 回缩放）。
    // 用户偏好不被写入：_tierOverride 是会话级，动设置或换集即清除。
    final cheaper =
        selected.backend == VideoEnhancementBackend.anime
            ? nextCheaperTier(_effectiveTier)
            : null;
    if (cheaper != null) {
      _tierOverride = cheaper;
      _downgradeReason = '本集已为保持流畅降档到${cheaper.label}';
    } else {
      _costLimit = math.min(
        _costLimit,
        selected.backend == VideoEnhancementBackend.hardware
            ? 2
            : selected.backend.cost - 1,
      );
      _downgradeReason = '本集已为保持流畅降低增强';
    }
    ignorePerformance();
    _schedule(immediate: true);
  }

  void _publishStatus() {
    final selected = _applied;
    if (selected == null) return;
    if (_comparing) {
      _setStatus('原画对比中');
    } else if (selected.backend == VideoEnhancementBackend.original) {
      _setStatus(
        _lastFailure.isNotEmpty && _preferences.mode != VideoEnhancementMode.off
            ? '增强不可用，已恢复原画'
            : selected.reason,
      );
    } else if (!_confirmed) {
      _setStatus('正在确认${selected.backend.label}');
    } else if (selected.backend == VideoEnhancementBackend.hardware) {
      _setStatus('硬件增强已接入，驱动效果以对比为准');
    } else {
      _setStatus(
        [
          selected.backend.label,
          selected.reason,
        ].where((item) => item.isNotEmpty).join(' · '),
      );
    }
  }

  void _setStatus(String value) {
    if (_closed || value == _status) return;
    _status = value;
    notifyListeners();
  }

  Future<void> close() => _closing ??= _close();

  Future<void> _close() async {
    if (_closed) return;
    _closed = true;
    _revision++;
    _timer?.cancel();
    _debounce?.cancel();
    for (final subscription in _subscriptions) {
      await subscription.cancel();
    }
    await _initialization;
    await _tail;
    await _monitoring;
    await _output?.close();
    if (_mpv != null) _mpv!.closed = true;
    super.dispose();
  }
}
