/// This file is a part of media_kit (https://github.com/media-kit/media-kit).
///
/// Copyright © 2021 & onwards, Hitesh Kumar Saini <saini123hitesh@gmail.com>.
/// All rights reserved.
/// Use of this source code is governed by MIT license that can be found in the LICENSE file.
import 'dart:math' as math;
import 'dart:ui';

/// {@template video_fixed_surface}
///
/// 固定输出表面策略（guoguo fork 新增，上游 media_kit_video 无此概念）。
///
/// ## 背景：安卓端为什么需要它
///
/// 上游 `VideoControllerConfiguration.width` / `height` 在安卓端从未被读取：
/// 安卓的渲染表面尺寸由 `AndroidVideoController` 的 `videoParamsSubscription`
/// 在每次 videoParams 事件上调用 `VideoOutputManager.SetSurfaceSize` 决定，
/// 恒等于视频显示尺寸 dw×dh。后果是 Anime4K 链里所有 Upscale pass 的
/// `WHEN` 条件（OUTPUT ≡ MAIN）不成立，被 mpv 整段跳过，安卓端只剩同分辨率
/// 重建/降噪——想拿到真正的 1080p→2K 放大，必须让渲染目标大于源尺寸。
///
/// ## 本类做什么
///
/// 宿主应用在创建 `VideoController` 时一次性给出"放大策略"（纯数据，可拷贝、
/// 可比较），fork 在**首次** videoParams 事件上按策略把表面放大到
/// 「源尺寸 × 倍率」（受像素/单边上限约束），随后锁定、永不 resize。
/// 沿用上游首次 `SetSurfaceSize` 的调用时机，不新增任何调用点，因此不会引入
/// 真机验证过的三个连锁问题（VO 重建 + seek 转圈、与 media_kit 自身
/// videoParams 监听乒乓、误触增强自愈把后端永久拉黑）。
///
/// ## 为什么倍率按"源尺寸"而不是"屏幕尺寸"计算
///
/// mpv 在表面内按 `keepaspect` 对视频做 letterbox，Flutter 侧 `Video` 组件又用
/// `FittedBox(BoxFit.contain)` 把表面贴进控件。若表面取屏幕尺寸（如竖屏
/// 1080×2400）而视频是 16:9，视频会被"mpv 黑边 + Flutter contain"双层挤压，
/// 变成画面中央一小块。只有保持视频宽高比的放大才能既超分又不变形。
///
/// {@endtemplate}
class VideoFixedSurface {
  /// {@macro video_fixed_surface}
  const VideoFixedSurface({
    this.maxScale = 2.0,
    this.maxPixels = 3686400,
    this.maxDimension = 2560,
  });

  /// 相对源尺寸的最大放大倍率。
  ///
  /// Anime4K 链内部按 2× 源渲染，默认 2.0 与其对齐；配合 [maxPixels] 后，
  /// 1080p 片源实际落到 1.33×（2560×1440），仍大于源尺寸、Upscale pass 生效。
  final double maxScale;

  /// 表面像素上限，防止超大源无限放大打爆显存与填充率。
  ///
  /// 默认 3686400（2560×1440，即"2K"级别）。
  final double maxPixels;

  /// 表面单边像素上限，兜底极端宽高比（如 9:32 竖屏短片）。
  final double maxDimension;

  /// 由视频显示尺寸（dw×dh，已应用旋转）计算固定表面尺寸。
  ///
  /// 倍率 = `min(maxScale, sqrt(maxPixels / 源像素), maxDimension / 源长边)`，
  /// 并夹紧到 `[1, maxScale]`：源本身超过上限时倍率退化为 1，表面即源尺寸，
  /// 与上游默认行为完全一致（此时不应期待 Upscale pass 生效）。
  ///
  /// 结果按偶数对齐（mediacodec / GL 表面普遍要求偶数尺寸），非法输入返回
  /// [Size.zero]（调用方应忽略并交还原动态逻辑）。
  Size resolve(Size source) {
    final width = source.width;
    final height = source.height;
    if (width <= 0 || height <= 0 || !width.isFinite || !height.isFinite) {
      return Size.zero;
    }
    final scale = math.min(
      maxScale,
      math.min(
        math.sqrt(maxPixels / (width * height)),
        maxDimension / math.max(width, height),
      ),
    );
    // 夹紧到 [1, maxScale]：超大源不缩表面（缩了只会白丢画质），
    // maxScale 本身也兜住不可能出现的负值/零。
    final clamped = math.min(math.max(scale, 1.0), math.max(maxScale, 1.0));
    return Size(
      math.max(2, (width * clamped).round() ~/ 2 * 2).toDouble(),
      math.max(2, (height * clamped).round() ~/ 2 * 2).toDouble(),
    );
  }

  VideoFixedSurface copyWith({
    double? maxScale,
    double? maxPixels,
    double? maxDimension,
  }) => VideoFixedSurface(
    maxScale: maxScale ?? this.maxScale,
    maxPixels: maxPixels ?? this.maxPixels,
    maxDimension: maxDimension ?? this.maxDimension,
  );

  @override
  bool operator ==(Object other) =>
      other is VideoFixedSurface &&
      other.maxScale == maxScale &&
      other.maxPixels == maxPixels &&
      other.maxDimension == maxDimension;

  @override
  int get hashCode => Object.hash(maxScale, maxPixels, maxDimension);

  @override
  String toString() =>
      'VideoFixedSurface(maxScale: $maxScale, maxPixels: $maxPixels, maxDimension: $maxDimension)';
}
