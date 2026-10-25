import 'dart:ui';

import 'package:flutter_test/flutter_test.dart';
import 'package:media_kit_video/media_kit_video.dart';

/// [VideoFixedSurface]（fork packages/media_kit_video）的倍率公式回归。
///
/// 该公式是安卓端"真超分"的唯一入口：fork 在首次 videoParams 事件上按它把
/// 渲染表面一次性放大到「源尺寸 × 倍率」并锁定，让 Anime4K 的 Upscale pass
/// 真正参与渲染（OUTPUT > MAIN）。这里固定：
///   - 1080p → 2560×1440（像素上限 3686400 生效，倍率 4/3）；
///   - 720p / 540p → 倍率 2.0 封顶；
///   - 超大源（4K）不缩表面，倍率夹紧到 1（退回上游默认语义）；
///   - 奇数源按偶数对齐；非法输入返回 Size.zero。
void main() {
  group('VideoFixedSurface.resolve', () {
    // 与 lib/video_output_size.dart 的 androidFixedSurfacePolicy 同参数。
    const policy = VideoFixedSurface(
      maxScale: 2.0,
      maxPixels: 3686400,
      maxDimension: 2560,
    );

    test('1080p → 2560×1440（像素上限生效，倍率 4/3）', () {
      expect(policy.resolve(const Size(1920, 1080)), const Size(2560, 1440));
    });

    test('720p → 2560×1440（倍率封顶 2.0）', () {
      expect(policy.resolve(const Size(1280, 720)), const Size(2560, 1440));
    });

    test('960×540 → 1920×1080（倍率 2.0，未触像素上限）', () {
      expect(policy.resolve(const Size(960, 540)), const Size(1920, 1080));
    });

    test('544×960 竖剧 → 1088×1920', () {
      expect(policy.resolve(const Size(544, 960)), const Size(1088, 1920));
    });

    test('4K 源不缩表面：倍率夹紧到 1，表面=源尺寸', () {
      expect(policy.resolve(const Size(3840, 2160)), const Size(3840, 2160));
    });

    test('极端宽高比受单边上限约束（480×1920 → 640×2560）', () {
      expect(policy.resolve(const Size(480, 1920)), const Size(640, 2560));
    });

    test('奇数源按偶数对齐（961×541 → 1922×1082）', () {
      expect(policy.resolve(const Size(961, 541)), const Size(1922, 1082));
    });

    test('非法输入返回 Size.zero', () {
      expect(policy.resolve(Size.zero), Size.zero);
      expect(policy.resolve(const Size(-1, 100)), Size.zero);
      expect(policy.resolve(const Size(100, 0)), Size.zero);
      expect(policy.resolve(const Size(double.nan, 100)), Size.zero);
      expect(policy.resolve(const Size(double.infinity, 100)), Size.zero);
    });

    test('copyWith / == / hashCode / toString', () {
      expect(policy.copyWith(), policy);
      expect(policy.copyWith(maxScale: 1.5), isNot(policy));
      expect(
        policy ==
            const VideoFixedSurface(
              maxScale: 2.0,
              maxPixels: 3686400,
              maxDimension: 2560,
            ),
        isTrue,
      );
      expect(
        policy.hashCode,
        const VideoFixedSurface(
          maxScale: 2.0,
          maxPixels: 3686400,
          maxDimension: 2560,
        ).hashCode,
      );
      expect(policy.toString(), contains('VideoFixedSurface('));
    });
  });
}
