import 'dart:io';
import 'dart:ui';

import 'package:media_kit/media_kit.dart';
import 'package:media_kit_video/media_kit_video.dart';

Size? videoDisplaySize(VideoParams parameters) {
  final width = (parameters.dw ?? 0) > 0 ? parameters.dw! : parameters.w ?? 0;
  final height = (parameters.dh ?? 0) > 0 ? parameters.dh! : parameters.h ?? 0;
  if (width <= 0 || height <= 0) return null;
  final rotation = (parameters.rotate ?? 0) % 180;
  return rotation == 90
      ? Size(height.toDouble(), width.toDouble())
      : Size(width.toDouble(), height.toDouble());
}

/// 安卓固定输出表面的默认策略（实现见 media_kit_video 的 [VideoFixedSurface]）。
///
/// 取值口径：
/// - `maxScale 2.0`：与 Anime4K 链的 2× 内部渲染对齐；
/// - `maxPixels 3686400`（2560×1440）：1080p 片源可放大到 2K 级别，
///   同时挡住 4K 片源无限放大；
/// - `maxDimension 2560`：极端宽高比（如 9:32 竖屏短片）的单边兜底。
///
/// 调大 maxPixels / maxDimension 可换取更高上限的表面，代价是填充率与显存；
/// 真正的瓶颈通常在 video_enhancement_policy.dart 的安卓显存预算（128MB），
/// 源分辨率越大越容易被预算拦下（见 README「画质增强」章节）。
const androidFixedSurfacePolicy = VideoFixedSurface(
  maxScale: 2.0,
  maxPixels: 3686400,
  maxDimension: 2560,
);

/// 画质增强的"输出表面尺寸"适配器。
///
/// ## 平台差异（2026-10 移植修复的核心结论）
///
/// 桌面端（Windows/macOS/Linux）与安卓端修改视频输出尺寸的路径完全不同：
///
/// - **桌面端**：`VideoController.setSize()` → `VideoOutputManager.SetSize`
///   → 原生 C++ 直接改渲染目标尺寸。不动 VO、不 seek、不碰 demuxer，
///   没有任何播放副作用，因此上游（Windows 独占版）可以随意放大表面，
///   让 Anime4K 的 2× 内部渲染以放大后的表面呈现，换取额外锐度。
/// - **安卓端**：`AndroidVideoController.setSize()` 直接抛
///   `UnsupportedError`（media_kit_video 2.0.1
///   android_video_controller/real.dart:220-228），原先只能改道
///   `VideoOutputManager.SetSurfaceSize` MethodChannel。
///
/// ## 为什么安卓端最终选择"完全不放大表面"
///
/// 真机验证发现 `SetSurfaceSize` 路径有三个连锁问题：
///
/// 1. **VO 重建 + seek → 转圈**：
///    `VideoOutput.setSurfaceSize`（media_kit_video 2.0.1
///    VideoOutput.java:75-101）会重设 SurfaceTexture 缓冲尺寸并回调
///    `onSurfaceAvailable()`，回传 `VideoOutput.Resize`；一旦 wid 指针变化，
///    `AndroidVideoController.widListener`（同包 real.dart:49-76）执行
///    `vo=null` → 重设 `android-surface-size`/`wid`/`vo` →
///    `player.seek(当前位置)`。seek 使 mpv `core-idle` /
///    `paused-for-cache` 置位，media_kit 据此把 `state.buffering` 置 true
///    （media_kit-1.2.6 native/player/real.dart:1517/1536），
///    播放器界面随即画出转圈图标（player_controls.dart:254）。
/// 2. **与 media_kit 自身监听乒乓**：media_kit 自带的 `videoParamsSubscription`
///    （real.dart:87-129）会在每次 videoParams 事件时把表面重置回视频显示
///    尺寸 dw×dh；增强侧又强制放大，形成"每次 videoParams 事件翻转两次
///    表面"的循环，无法收敛，表现为"转圈→停止→再转圈"。
/// 3. **误触增强自愈 → 后端被永久拉黑**：抖动期间 mpv 打出 vo/gpu 相关
///    告警，被 video_enhancement.dart 的 `_onLog` /
///    `handlePlaybackError` 判定为"增强渲染失败"而 `_failBackend`；
///    `_rejected` / `_costLimit` 整集有效，导致进全屏后神经超分再也不回来。
///
/// ## 为什么不放大会表面也有超分效果
///
/// Anime4K 链（+87 起为精细档 7 文件链，+144 前名轻量档，见
/// lib/video_enhancement_assets.dart 的 animeChain）按 WHEN 指令分平台生效：
/// 默认形态下安卓端表面恒为视频显示尺寸（OUTPUT≡MAIN），mpv 自动跳过全部
/// Upscale / AutoDownscalePre pass，仅 Clamp_Highlights + Restore_CNN 生效——
/// 这是**同分辨率重建/降噪**（去压缩噪点、重建线条），也是安卓端默认
/// 唯一生效的形态；桌面端表面仍放大 2×（WHEN 为真），Upscale 链全生效，
/// 内部按 2× 源渲染、Depth-to-Space 输出后由 mpv 缩放到表面。
/// 两条路径都只 `change-list glsl-shaders set`，不动表面尺寸。
///
/// ## 默认形态下如何放大表面（已落地：VideoFixedSurface）
///
/// 真机验证过：播放中改表面必踩上方三个问题，因此放大只能在
/// `VideoController` 创建时一次性指定——宿主给
/// `VideoControllerConfiguration.fixedSurface` 传一个
/// [VideoFixedSurface] 策略（纯数据），fork 在**首次** videoParams 事件上
/// 按「源尺寸 × 倍率」放大表面并锁定，不新增任何调用时机，三个连锁问题
/// 均不会触发。倍率按源尺寸（而非屏幕尺寸）计算以保持视频宽高比，否则
/// mpv 黑边 + Flutter `FittedBox(contain)` 会把视频挤成中央一小块。
///
/// 约束（均已在代码/注释中固化）：
/// - 表面只在播放器创建时锁定一次。竖屏进入后旋转横屏不会重新放大
///   （重新 SetSurfaceSize 就是播放中改表面，会转圈），该次会话只剩
///   同分辨率重建；中途把增强从「关」改到「开」同理，需退出重进播放器。
/// - 放大表面后，video_enhancement.dart 的 `_monitor` 用
///   `expectsUpscale` 强制要求 Anime4K 链出现 `Depth-to-Space` 末 pass
///   （极简档无 Upscale 链，不强制，否则必误报 render-pass），防止
///   Upscale 仍被 WHEN 跳过却误报成功。
/// - 增强策略侧（video_enhancement_policy.dart）拿到的 surfaceOutput 即
///   实际表面尺寸，与 fork 应用的值恒等，sizeMatches 才不会误拉黑后端。
class VideoOutputSizeAdapter {
  VideoOutputSizeAdapter(this.video, {required this.onFailure});

  final VideoController video;

  /// 表面尺寸操作失败时的回调。
  ///
  /// [移植改造] 安卓端放弃放大表面后此回调不再被触发（桌面端 setSize 的异常
  /// 由 VideoEnhancementController._enqueue 统一捕获）。保留形参以维持调用方
  /// （video_enhancement.dart）构造签名不变。
  final void Function() onFailure;
  Future<void>? _initialization;
  Size? _target;
  bool _closed = false;

  /// 当前实际表面尺寸（mpv/Flutter 侧上报，非增强期望值）。
  Size? get actualSize => video.rect.value?.size;

  Future<void> initialize() => _initialization ??= _initialize();

  Future<void> _initialize() async {
    // 等待平台侧 VideoController 就绪即可；安卓端不再注册任何 videoParams
    // 监听——原先在此监听是为了在 media_kit 把表面重置回 dw×dh 后重新放大，
    // 与 media_kit 自身的 videoParamsSubscription 形成乒乓，是播放中转圈与
    // 超分被拉黑的直接原因，详见类注释。
    await video.platform.future;
  }

  /// 设置增强期望的输出表面尺寸。
  ///
  /// [移植改造] 安卓端整体为 no-op：表面尺寸交给 media_kit 自身维护
  /// （其 videoParamsSubscription 会把表面同步为视频显示尺寸 dw×dh；宿主
  /// 启用固定表面后同步为「源尺寸 × 倍率」的一次性锁定值，见类注释）。
  /// 期望值仍记录在 [_target] 中供诊断/将来使用，但绝不触碰表面。
  /// 桌面端行为与上游完全一致（仅在尺寸真正变化时调用 setSize）。
  Future<void> setTarget(Size? value) async {
    await initialize();
    if (_closed) return;
    final normalized = value == null
        ? null
        : Size(value.width.roundToDouble(), value.height.roundToDouble());
    final changed = _target != normalized;
    _target = normalized;
    if (Platform.isAndroid) {
      // 安卓：仅记录，绝不调用 VideoOutputManager.SetSurfaceSize。
      // 原因（VO 重建 + seek → 转圈、与 media_kit 乒乓、误触自愈拉黑）
      // 详见类注释。
      return;
    }
    if (changed || normalized == null) {
      await video.setSize(
        width: normalized?.width.toInt(),
        height: normalized?.height.toInt(),
      );
    }
  }

  Future<void> close() async {
    _closed = true;
    await _initialization;
  }
}
