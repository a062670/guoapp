import 'dart:math' as math;
import 'dart:ui';

import 'package:media_kit/media_kit.dart';

import 'video_enhancement_preferences.dart';
import 'video_output_size.dart';

enum VideoEnhancementBackend {
  original(0, '原画'),
  scaling(1, '高质量缩放'),
  ravu(2, '轻量增强'),
  fsrcnnx(3, '通用神经超分'),
  anime(3, '动漫神经超分'),
  hardware(4, '硬件增强');

  const VideoEnhancementBackend(this.cost, this.label);
  final int cost;
  final String label;
}

/// [移植改造] 安卓端允许使用的最大增强成本档位。
///
/// 取值说明：3 恰好等于 `anime`（Anime4K 动漫神经超分）与 `fsrcnnx`（通用神经超分）的成本；
/// 桌面端为 4（额外允许 `hardware` 硬件增强）。之所以不直接给安卓 4，是因为
/// `hardware` 分支依赖 d3d11va 等 Windows 专有通路径，安卓不适用。
///
/// 调整此处即为安卓端画质/性能的全局开关：调到 2 可退回"仅 ravu 轻量增强"的保守行为。
const int kAndroidBackendCostLimit = 3;

/// [移植改造] 温控降级门槛：`PowerManager.currentThermalStatus` 达到该等级才降级。
///
/// 等级对照：NONE(0) / LIGHT(1) / MODERATE(2) / SEVERE(3) / CRITICAL(4) / EMERGENCY(5) / SHUTDOWN(6)
/// 原始值为 2，即 MODERATE 就降级。
///
/// 实测问题：真机（Android 12+）在未开省电模式、未明显发热的情况下仍稳定触发降级，
/// Anime4K 几乎无法持续运行。常见原因是厂商 ROM 对 currentThermalStatus 上报偏保守。
///
/// 放宽到 3（SEVERE）后，仍有 CRITICAL / EMERGENCY / SHUTDOWN 三级余量，
/// 且还有独立防线兜底：video_enhancement.dart 的 _monitor() 会按实际掉帧采样
/// 动态收敛 _costLimit 并熔断后端（performance 层保护，不依赖温控上报）。
///
/// ⚠️ 若出现机身明显发烫、掉帧加剧或耗电异常，改回 2 即可恢复上游保守行为。
const int kThermalStatusLimit = 3;

/// [移植改造] 热余量降级门槛：`PowerManager.getThermalHeadroom(0)` 阈值。
///
/// 取值语义：0.0 = 无性能限制，1.0 = 已限制到最低频率。
/// 原始值为 0.85。
///
/// 同理放宽：部分中高端机该预测值常态偏高，0.85 过于敏感。
/// 注意该值在 MainActivity.kt 中每 10 秒才刷新一次，存在滞后性。
///
/// ⚠️ 改回 0.85 可恢复上游保守行为。
const double kThermalHeadroomLimit = 0.95;

/// [移植改造] 神经超分（Anime4K / FSRCNNX）允许的最高播放倍速，含边界值。
///
/// 原始值为 `rate < 1.5`，即 ≥1.5x 就失去神经超分资格；由于该函数位于 anime 分支之前，
/// 实际效果是「1.5x 及以上永远拿不到 Anime4K」，且界面不会给出任何提示。
/// 放宽到 2.0 后，常用倍速（1.5x / 1.75x / 2x）仍可使用 Anime4K。
///
/// ⚠️ 高倍速意味着单位时间需处理的帧数成倍增加，叠加超分会显著提高 GPU 负载。
///    若出现掉帧或发热，调低本值；设为 0 即完全禁用倍速下的神经超分。
const double kNeuralMaxRate = 2.0;

/// [移植改造] 完全保留画质增强允许的最高播放倍速，超过才开始降级。
///
/// 原始值为 `rate >= 1.75`，即 ≥1.75x 直接回落到高质量缩放（对应提示「倍速播放，已降低增强」）。
/// 放宽到 2.5 后，≤2.5x 都不会因倍速降级。
///
/// ⚠️ 超过本值后画面并非立刻回到原画，而是交由 ravu / scaling 承担，
///    加之 video_enhancement.dart 的 _monitor() 掉帧熔断，形成多级兜底。
///    设为 double.infinity 可彻底取消倍速限制（不推荐）。
const double kEnhancedMaxRate = 2.5;

class VideoEnhancementPower {
  const VideoEnhancementPower({
    this.batterySaver = false,
    this.onBattery = true,
    this.thermalStatus = 0,
    this.headroom,
    this.lowMemory = false,
    this.gles = 0,
  });
  final bool batterySaver;
  final bool onBattery;
  final int thermalStatus;
  final double? headroom;
  final bool lowMemory;
  final int gles;

  factory VideoEnhancementPower.fromMap(Map value) {
    final headroom = (value['headroom'] as num?)?.toDouble();
    return VideoEnhancementPower(
      batterySaver: value['batterySaver'] == true,
      onBattery: value['onBattery'] != false,
      thermalStatus: (value['thermalStatus'] as num?)?.toInt() ?? 0,
      headroom: headroom != null && headroom.isFinite ? headroom : null,
      lowMemory: value['lowMemory'] == true,
      gles: (value['gles'] as num?)?.toInt() ?? 0,
    );
  }
}

class VideoEnhancementDecision {
  const VideoEnhancementDecision(this.backend, this.output, this.reason);
  final VideoEnhancementBackend backend;
  final Size? output;
  final String reason;

  @override
  bool operator ==(Object other) =>
      other is VideoEnhancementDecision &&
      backend == other.backend &&
      output == other.output &&
      reason == other.reason;

  @override
  int get hashCode => Object.hash(backend, output, reason);
}

/// 性能看门狗阶梯降档用的「下一个更便宜档位」（+142）。
///
/// 依赖 [VideoEnhancementTier] 的声明顺序即算力升序（见该枚举的文档约定）：
/// 极简 → 均衡 → 标准 → 轻量 → 质量。看门狗确认过载后逐级下移，到极简仍
/// 过载才由调用方放弃 anime 后端（降 costLimit 回落高质量缩放），避免
/// 「一卡顿就整个失去增强」的断崖式体验。
///
/// 返回 null 表示已在最低档（极简），没有更便宜的档可降。
VideoEnhancementTier? nextCheaperTier(VideoEnhancementTier tier) {
  final index = VideoEnhancementTier.values.indexOf(tier);
  return index > 0 ? VideoEnhancementTier.values[index - 1] : null;
}

bool videoHasAnimationTags(String category, List<String> tags) => RegExp(
  r'漫剧|动漫|动画|二次元|\banime\b|\banimation\b|comic_series',
  caseSensitive: false,
).hasMatch([category, ...tags].join(' '));

String videoEnhancementPixelFormat(VideoParams parameters) {
  final hardware = parameters.hwPixelformat?.trim() ?? '';
  return (hardware.isNotEmpty ? hardware : parameters.pixelformat ?? '')
      .toLowerCase();
}

bool videoHasSafeEnhancementColor(
  VideoParams parameters, {
  bool opaque8Bit = false,
}) {
  final gamma = parameters.gamma?.toLowerCase() ?? '';
  final primaries = parameters.primaries?.toLowerCase() ?? '';
  final format = videoEnhancementPixelFormat(parameters);
  return {
        'bt.1886',
        'srgb',
        'gamma1.8',
        'gamma2.2',
        'gamma2.8',
      }.contains(gamma) &&
      {
        'bt.709',
        'bt.601-525',
        'bt.601-625',
        'bt.470bg',
        'smpte-170m',
      }.contains(primaries) &&
      (opaque8Bit && format == 'mediacodec' ||
          {
            'yuv420p',
            'yuvj420p',
            'yuv422p',
            'yuvj422p',
            'yuv444p',
            'yuvj444p',
            'nv12',
            'nv21',
            'yuyv422',
            'uyvy422',
            'rgb0',
            'bgr0',
            '0rgb',
            '0bgr',
            'rgba',
            'bgra',
            'rgb24',
            'bgr24',
          }.contains(format)) &&
      (parameters.sigPeak == null || parameters.sigPeak! <= 1.01);
}

VideoEnhancementDecision chooseVideoEnhancement({
  required VideoEnhancementPreferences preferences,
  required VideoParams parameters,
  required Size viewport,
  required bool ready,
  required bool android,
  required bool television,
  required bool gpu,
  required bool luma,
  required bool hardware,
  required bool animation,
  required VideoEnhancementPower power,
  required double rate,
  required int costLimit,
  required Set<VideoEnhancementBackend> rejected,
  Size? fixedSurface,
  bool opaque8Bit = false,
}) {
  const original = VideoEnhancementBackend.original;
  if (preferences.mode == VideoEnhancementMode.off) {
    return const VideoEnhancementDecision(original, null, '画质增强已关闭');
  }
  if (television) {
    return const VideoEnhancementDecision(original, null, '电视端原生硬件直通，确保极速流畅');
  }
  final source = videoDisplaySize(parameters);
  if (!ready ||
      source == null ||
      viewport.isEmpty ||
      !viewport.width.isFinite ||
      !viewport.height.isFinite) {
    return const VideoEnhancementDecision(original, null, '等待播放画面');
  }
  // [结构重构·2026-10] 尺寸计算整体上移：高级强制档需要 surfaceOutput，
  // 且按用户「全部绕过、彻底强制」的决策，必须排在色彩/GPU/温控预检之前。
  // 本段是纯数学计算、无副作用，上移不改变自动族路径的语义与取值。
  final targetScale = math.min(
    viewport.width / source.width,
    viewport.height / source.height,
  );
  final maxPixels = android || television ? 2073600.0 : 8294400.0;
  final maxDimension = android || television ? 4096.0 : 8192.0;
  final scale = math.min(
    targetScale,
    math.min(
      2.0,
      math.min(
        math.sqrt(maxPixels / (source.width * source.height)),
        maxDimension / math.max(source.width, source.height),
      ),
    ),
  );
  if (scale <= 0) {
    return const VideoEnhancementDecision(original, null, '等待显示尺寸');
  }
  final output = Size(
    math.max(2, (source.width * scale).round() ~/ 2 * 2).toDouble(),
    math.max(2, (source.height * scale).round() ~/ 2 * 2).toDouble(),
  );
  // [移植改造·安卓专用] 安卓端默认不放大渲染表面（原因详见 video_output_size.dart
  /// 类注释：放大表面会引发 media_kit 的 VO 重建 + seek 转圈、与 media_kit 自身
  /// videoParams 监听乒乓、并误触增强自愈把后端拉黑）。因此返回给调用方的
  /// output 必须是"实际表面尺寸"——安卓默认即视频显示尺寸，桌面端仍为放大尺寸；
  /// 宿主启用固定表面后，安卓表面为一次性锁定的放大值（由 fixedSurface 传入），
  /// 语义不变：output 恒为实际表面。
  /// video_enhancement.dart 的 _monitor 用 output 与实际表面
  /// （rect / osd-dimensions）比对来确认增强生效；安卓若返回放大尺寸，
  /// sizeMatches 永远不成立，连续 5 次即 _failBackend 拉黑后端。
  final surfaceOutput = fixedSurface ?? (android ? source : output);
  // [结构重构·2026-10] 高级档：用户明确要求"直接开启 Anime4K"，全部绕过
  // 自动判断——动画标签、温控/headroom、播放倍速、显存预算、targetScale、
  // rejected 拉黑、色彩/GPU/GLES 预检一概不看。
  // 保留的底线：上方 source/viewport 就绪检查（没有画面时无从增强）；
  // 运行时若 shader 编译/渲染失败，由 VideoEnhancementController 的
  // _onLog/handlePlaybackError -> _failBackend 兜底（4 次后熔断暂停），
  // 不依赖本函数预判。
  // 安卓端默认表面恒为源尺寸，scale/maxPixels 上限对渲染无实际影响（shader
  // 内部恒按 2× 源渲染）；固定表面启用后表面大于源、Upscale 链生效，显存
  // 预算改按 surfaceOutput 计入（见下方 neuralMemory）。桌面端沿用放大的
  // output，与上游语义一致。
  if (preferences.mode == VideoEnhancementMode.advanced) {
    return VideoEnhancementDecision(
      VideoEnhancementBackend.anime,
      surfaceOutput,
      '高级模式：强制 Anime4K 超分',
    );
  }
  // 以下预检仅对自动族生效（高级档已在上面直接返回，不看这些条件）。
  if (!videoHasSafeEnhancementColor(parameters, opaque8Bit: opaque8Bit)) {
    return const VideoEnhancementDecision(original, null, '当前色彩或位深保持原画');
  }
  if (!gpu) {
    return const VideoEnhancementDecision(original, null, '当前渲染环境保持原画');
  }
  if (costLimit < 1 ||
      rejected.contains(VideoEnhancementBackend.scaling) ||
      power.thermalStatus >= 3 ||
      (power.headroom ?? 0) >= 1) {
    return const VideoEnhancementDecision(original, null, '为保持流畅，已暂停增强');
  }
  final scaling = VideoEnhancementDecision(
    VideoEnhancementBackend.scaling,
    surfaceOutput,
    targetScale <= 1 ? '当前尺寸无需超分，使用高质量缩放' : '',
  );
  final ratio = math.min(
    output.width / source.width,
    output.height / source.height,
  );
  // [移植改造·安卓专用] ratio 是"表面放大倍率"代理；安卓默认表面恒为源尺寸、
  /// 该代理失效，是否值得上神经超分改看"显示端需要的放大倍率" targetScale
  /// （视口/源）——固定表面启用后表面虽大于源，但神经增强的收益仍取决于显示
  /// 端是否需要放大，故继续用 targetScale，与表面放大倍率保持解耦。
  /// 桌面端语义不变（仍看 ratio）。
  final upscale = android ? targetScale : ratio;
  if (upscale <= 1.2 || preferences.mode == VideoEnhancementMode.economy) {
    return scaling;
  }
  // [结构重构·2026-10] 原「手机自动档优先流畅」分支随 mode.automatic
  // 一并移除：旧 automatic 已迁移为 economy（省电增强），由上一行
  // `mode == economy` 分支承接；需要 Anime4K 的用户改用清晰优先或高级档。
  // [移植改造] 原始实现为：
  //     power.thermalStatus >= 2
  //     (power.headroom ?? 0) >= .85
  // 这两个硬编码阈值在真机（Android 12+、省电模式已关闭）上过于敏感，导致
  // Anime4K 刚启用就被降级。现改为具名常量 kThermalStatusLimit / kThermalHeadroomLimit，
  // 详见其定义处的等级说明与回退方式。
  //
  // 保留的上游保护：batterySaver（用户手动开了省电就该降级）。
  // 放宽项：原 `rate >= 1.75` 改为 `rate > kEnhancedMaxRate`（默认 2.5），
  // 使常用倍速不再被直接降级，详见常量定义处说明。
  if (power.batterySaver ||
      rate > kEnhancedMaxRate ||
      power.thermalStatus >= kThermalStatusLimit ||
      (power.headroom ?? 0) >= kThermalHeadroomLimit) {
    return VideoEnhancementDecision(
      VideoEnhancementBackend.scaling,
      surfaceOutput,
      rate > kEnhancedMaxRate ? '倍速播放，已降低增强' : '省电或温控限制，已降低增强',
    );
  }
  final squarePixels =
      (parameters.par ?? 1) > .99 && (parameters.par ?? 1) < 1.01;
  if (!squarePixels ||
      (parameters.rotate ?? 0) % 180 != 0 ||
      android && power.gles < 0x30000) {
    return scaling;
  }
  final pixels = (parameters.w ?? 0) * (parameters.h ?? 0);
  if (pixels <= 0) return scaling;
  // [移植改造] 原始实现为：
  //     final limit = math.min(costLimit, television || power.lowMemory || android ? 2 : 4);
  // gles 之外最隐蔽的一道门槛在这里：VideoEnhancementBackend 的成本分级为
  //   original(0) / scaling(1) / ravu(2) / fsrcnnx(3) / anime(3) / hardware(4)
  // 安卓被封顶到 cost <= 2，而 Anime4K（anime）与通用神经超分（fsrcnnx）成本都是 3，
  // 于是 `allowed(VideoEnhancementBackend.anime)` 在安卓上恒为 false，Anime4K 永远选不中。
  //
  // 现在把安卓放宽到 cost <= kAndroidBackendCostLimit(3)，使其能够命中 anime；
  // 电视端（需走原生硬件直通）与低内存机仍严格保持 2，避免内存与发热失控。
  final limit = math.min(
    costLimit,
    television || power.lowMemory
        ? 2
        : android
        ? kAndroidBackendCostLimit
        : 4,
  );
  bool allowed(VideoEnhancementBackend backend) =>
      backend.cost <= limit && !rejected.contains(backend);
  final memory = android ? (power.lowMemory ? 64 : 128) : 512;
  // [移植改造·安卓专用] 显存预算第二项（输出 FBO）按 surfaceOutput 计算：
  // 安卓端默认表面恒为源尺寸，而 Anime4K 链内部按 2× 源渲染，成本只与源分辨率
  // 相关、与视口无关。若沿用视口放大后的 output，全屏时预算会无谓爆掉，
  // 表现为"半屏能超分、全屏只剩高质量缩放"。桌面端 surfaceOutput == output，
  // 行为与上游一致。固定表面启用后 surfaceOutput 为锁定的大表面，预算随之
  // 抬高，超大表面会像大全屏视口一样把预算撑爆、自然回退缩放（见
  // test/video_enhancement_policy_test.dart 的固定表面 group）。
  final neuralMemory =
      (pixels * 8 * 18 + surfaceOutput.width * surfaceOutput.height * 8 * 3) /
      (1024 * 1024);
  // [移植改造] 原始实现为：
  //     final neuralSize = !android;
  //     final neural = neuralSize && neuralMemory <= memory && rate < 1.5;
  // 安卓被硬性排除在"神经超分"之外，导致下一句 `animation && neural && allowed(anime)`
  // 恒为 false，Anime4K 即使在 resources 与 costLimit 都放行的情况下也永远选不中。
  //
  // 放开方式：安卓同样参与神经超分候选，但**不绕过任何资源约束**——
  // 仍受显存预算 memory（安卓 128MB / 低内存机 64MB，桌面 512MB）与
  // GLES >= 3.0（`android && power.gles < 0x30000` 分支）的双重限制，
  // 内存不够或 GLES 版本不足时由这些条件自然回退，而不是在此处一刀切禁用。
  // [移植改造] 末端的 `rate < 1.5` 改为 `rate <= kNeuralMaxRate`（默认 2.0）：
  // 该条件直接决定能否进入下方 `animation && neural && allowed(anime)` 分支，
  // 原值会让 1.5x 及以上的播放**静默失去** Anime4K（界面无任何提示）。
  final neural = neuralMemory <= memory && rate <= kNeuralMaxRate;
  if (preferences.mode == VideoEnhancementMode.quality &&
      !animation &&
      hardware &&
      !power.onBattery &&
      allowed(VideoEnhancementBackend.hardware)) {
    return VideoEnhancementDecision(
      VideoEnhancementBackend.hardware,
      surfaceOutput,
      '驱动增强效果以原画对比为准',
    );
  }
  if (animation && neural && allowed(VideoEnhancementBackend.anime)) {
    return VideoEnhancementDecision(
      VideoEnhancementBackend.anime,
      surfaceOutput,
      '',
    );
  }
  if (preferences.mode == VideoEnhancementMode.quality &&
      !animation &&
      luma &&
      neural &&
      !android &&
      ratio > 1.31 &&
      allowed(VideoEnhancementBackend.fsrcnnx)) {
    return VideoEnhancementDecision(
      VideoEnhancementBackend.fsrcnnx,
      surfaceOutput,
      '',
    );
  }
  final ravuMemory =
      (pixels * 8 * 10 + surfaceOutput.width * surfaceOutput.height * 8 * 3) /
      (1024 * 1024);
  if (upscale > 1.42 &&
      ravuMemory <= memory &&
      allowed(VideoEnhancementBackend.ravu)) {
    return VideoEnhancementDecision(
      VideoEnhancementBackend.ravu,
      surfaceOutput,
      '',
    );
  }
  return scaling;
}
