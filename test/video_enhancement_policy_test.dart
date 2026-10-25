import 'dart:ui';

import 'package:duanju_app/video_enhancement_assets.dart';
import 'package:duanju_app/video_enhancement_policy.dart';
import 'package:duanju_app/video_enhancement_preferences.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:media_kit/media_kit.dart';

/// 画质增强策略的回归测试。
///
/// 重点固定 2026-10「安卓端放弃放大表面」修复后的语义：
///   - 安卓：决策里的 output 恒等于视频显示尺寸（源尺寸），表面不被触碰；
///   - 安卓：是否上神经超分只看 targetScale（视口/源），与表面放大倍率解耦；
///   - 安卓：显存预算只跟源分辨率相关，半屏/全屏决策一致（进全屏不重新 apply）；
///   - 桌面：行为与上游一致，output 仍为放大后的尺寸。
///   - 固定表面启用后：安卓 output 恒为固定表面（与视口/平台无关），见文末 group。
VideoParams params({required int width, required int height, int rotate = 0}) =>
    VideoParams(
      pixelformat: 'yuv420p',
      w: width,
      h: height,
      dw: width,
      dh: height,
      par: 1.0,
      primaries: 'bt.709',
      gamma: 'bt.1886',
      rotate: rotate,
    );

const _qualityAnime = VideoEnhancementPreferences(
  mode: VideoEnhancementMode.quality,
  content: VideoEnhancementContent.animation,
);

const _power = VideoEnhancementPower(gles: 0x30001);

VideoEnhancementDecision decide({
  required VideoParams parameters,
  required Size viewport,
  required bool android,
  VideoEnhancementPreferences preferences = _qualityAnime,
  VideoEnhancementPower power = _power,
  double rate = 1.0,
  Set<VideoEnhancementBackend> rejected = const {},
  int costLimit = 4,
  Size? fixedSurface,
}) => chooseVideoEnhancement(
  preferences: preferences,
  parameters: parameters,
  viewport: viewport,
  ready: true,
  android: android,
  television: false,
  gpu: true,
  luma: true,
  hardware: false,
  animation: true,
  power: power,
  rate: rate,
  costLimit: costLimit,
  rejected: rejected,
  fixedSurface: fixedSurface,
  opaque8Bit: false,
);

void main() {
  group('安卓端表面语义（2026-10 修复）', () {
    final source = params(width: 544, height: 960);

    test('半屏：动漫档命中 anime，但 output 为源尺寸（不放大表面）', () {
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, const Size(544, 960));
    });

    test('全屏与半屏决策一致：进全屏不会重新 apply，无表面 churn', () {
      final half = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
      );
      final full = decide(
        parameters: source,
        viewport: const Size(1080, 2400),
        android: true,
      );
      expect(full, half);
      expect(full.backend, VideoEnhancementBackend.anime);
      expect(full.output, const Size(544, 960));
    });

    test('显示端不需要放大（targetScale<=1）时走高质量缩放', () {
      final decision = decide(
        parameters: source,
        viewport: const Size(544, 960),
        android: true,
      );
      expect(decision.backend, VideoEnhancementBackend.scaling);
      expect(decision.output, const Size(544, 960));
      expect(decision.reason, '当前尺寸无需超分，使用高质量缩放');
    });

    test('1080p 竖剧全屏：无需放大，走高质量缩放', () {
      final decision = decide(
        parameters: params(width: 1080, height: 1920),
        viewport: const Size(1080, 2400),
        android: true,
      );
      expect(decision.backend, VideoEnhancementBackend.scaling);
      expect(decision.output, const Size(1080, 1920));
      expect(decision.reason, '当前尺寸无需超分，使用高质量缩放');
    });

    test('源分辨率超出安卓显存预算时不选 anime（预算只看源，与视口无关）', () {
      final big = params(width: 890, height: 1600);
      for (final viewport in const [Size(1080, 1344), Size(1080, 2400)]) {
        final decision = decide(
          parameters: big,
          viewport: viewport,
          android: true,
        );
        expect(
          decision.backend,
          VideoEnhancementBackend.scaling,
          reason: '视口 $viewport 下大源不应命中 anime',
        );
        expect(decision.output, const Size(890, 1600));
      }
    });

    test('anime 被拉黑后本集回不去（rejected 持久化语义）', () {
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
        rejected: const {VideoEnhancementBackend.anime},
      );
      expect(decision.backend, VideoEnhancementBackend.scaling);
    });

    test('倍速超过 kNeuralMaxRate 后失去 anime 资格', () {
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
        rate: 2.5,
      );
      expect(decision.backend, VideoEnhancementBackend.scaling);

      final fast = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
        rate: 3.0,
      );
      expect(fast.backend, VideoEnhancementBackend.scaling);
      expect(fast.reason, '倍速播放，已降低增强');
    });

    test('常用倍速（1.75x）仍保留 anime', () {
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
        rate: 1.75,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
    });
  });

  group('桌面端行为不变', () {
    final source = params(width: 544, height: 960);

    test('output 仍为放大后的尺寸', () {
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: false,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, const Size(762, 1344));
    });

    test('桌面显存预算更宽（512MB），大源仍可选 anime', () {
      final decision = decide(
        parameters: params(width: 890, height: 1600),
        viewport: const Size(1080, 2400),
        android: false,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, const Size(1080, 1942));
    });
  });

  group('高级模式（2026-10 结构重构：彻底强制）', () {
    // 极端不利条件全部拉满：通用内容 + 省电 + 温控拉满 + headroom 满 +
    // 低内存 + 3 倍速 + anime 被拉黑 + 视口小于源（targetScale<1）。
    // 用户已确认「全部绕过、彻底强制」，以上均应一律返回 anime。
    const hostile = VideoEnhancementPower(
      gles: 0x30001,
      batterySaver: true,
      thermalStatus: 3,
      headroom: 1.0,
      lowMemory: true,
    );
    const advancedGeneral = VideoEnhancementPreferences(
      mode: VideoEnhancementMode.advanced,
      content: VideoEnhancementContent.general,
    );

    test('全部不利条件拉满仍强制 anime，output 为源尺寸', () {
      final decision = decide(
        parameters: params(width: 544, height: 960),
        viewport: const Size(272, 480), // targetScale=0.5
        android: true,
        preferences: advancedGeneral,
        power: hostile,
        rate: 3.0,
        rejected: const {VideoEnhancementBackend.anime},
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, const Size(544, 960));
      expect(decision.reason, '高级模式：强制 Anime4K 超分');
    });

    test('绕过色彩预检：非常见色彩空间仍强制 anime', () {
      final decision = decide(
        parameters: VideoParams(
          pixelformat: 'yuv420p10le',
          w: 544,
          h: 960,
          dw: 544,
          dh: 960,
          par: 1.0,
          primaries: 'bt.2020',
          gamma: 'pq',
          rotate: 0,
        ),
        viewport: const Size(1080, 1344),
        android: true,
        preferences: advancedGeneral,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
    });

    test('超大源仍强制 anime（安卓尺寸上限 inert，output=源尺寸）', () {
      final decision = decide(
        parameters: params(width: 4096, height: 2304),
        viewport: const Size(1080, 2400),
        android: true,
        preferences: advancedGeneral,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, const Size(4096, 2304));
    });

    test('桌面端高级：anime 且 output 仍为放大尺寸', () {
      final decision = decide(
        parameters: params(width: 544, height: 960),
        viewport: const Size(1080, 1344),
        android: false,
        preferences: advancedGeneral,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, const Size(762, 1344));
    });
  });

  group('偏好迁移（2026-10 结构重构）', () {
    test('旧 automatic 迁移为 economy（省电增强），零行为变化', () {
      expect(
        VideoEnhancementPreferences.fromJson({'mode': 'automatic'}).mode,
        VideoEnhancementMode.economy,
      );
    });

    test('advanced 往返序列化保持', () {
      const preferences = VideoEnhancementPreferences(
        mode: VideoEnhancementMode.advanced,
        content: VideoEnhancementContent.animation,
      );
      expect(
        VideoEnhancementPreferences.fromJson(preferences.toJson()),
        preferences,
      );
    });

    test('未知 mode 与非法值回落 off', () {
      expect(
        VideoEnhancementPreferences.fromJson({'mode': 'garbage'}).mode,
        VideoEnhancementMode.off,
      );
      expect(
        VideoEnhancementPreferences.fromJson(null).mode,
        VideoEnhancementMode.off,
      );
      expect(
        VideoEnhancementPreferences.fromJson('not-a-map').mode,
        VideoEnhancementMode.off,
      );
      expect(
        VideoEnhancementPreferences.fromJson({'mode': 'advanced'}).content,
        VideoEnhancementContent.automatic,
      );
    });
  });

  group('增强档位（2026-10 tier 新增）', () {
    test('默认 balanced，+144 起默认档由 lite 改为 balanced', () {
      expect(
        const VideoEnhancementPreferences().tier,
        VideoEnhancementTier.balanced,
      );
    });

    test('极简档是 Clamp + S 档重建，pass 数明显少于精细档', () {
      expect(VideoEnhancementAssets.animeChainMinimal, [
        'Anime4K_Clamp_Highlights.glsl',
        'Anime4K_Restore_CNN_S.glsl',
      ]);
      // 精细档在安卓端真正执行的是 Clamp + M + S，极简档只保留 S。
      expect(
        VideoEnhancementAssets.animeChainMinimal.length,
        lessThan(VideoEnhancementAssets.animeChainFine.length),
      );
      expect(
        VideoEnhancementAssets.animeChainMinimal,
        isNot(contains('Anime4K_Restore_CNN_M.glsl')),
      );
    });

    // [档位扩展·+142] 均衡/标准两档的链内容：均衡 = Clamp + S 重建 + S 超分
    // （有 Restore 兜底的最便宜真超分链），标准 = Clamp + M 重建 + S 超分。
    // 两者都必须带 Upscale_S——否则固定表面下会重蹈 +141 的覆辙（期望
    // Depth-to-Space 却拿不到，_monitor 误报 render-pass 拉黑后端）。
    test('均衡档是 Clamp + S 重建 + S 超分，且成本低于标准档', () {
      expect(VideoEnhancementAssets.animeChainBalanced, [
        'Anime4K_Clamp_Highlights.glsl',
        'Anime4K_Restore_CNN_S.glsl',
        'Anime4K_Upscale_CNN_x2_S.glsl',
      ]);
      expect(VideoEnhancementAssets.animeChainStandard, [
        'Anime4K_Clamp_Highlights.glsl',
        'Anime4K_Restore_CNN_M.glsl',
        'Anime4K_Upscale_CNN_x2_S.glsl',
      ]);
      // 均衡只多一个 S 重建文件；标准比均衡多一组 M 重建（6 层）。
      expect(
        VideoEnhancementAssets.animeChainStandard.length,
        VideoEnhancementAssets.animeChainBalanced.length,
      );
      expect(
        VideoEnhancementAssets.animeChainBalanced,
        isNot(contains('Anime4K_Restore_CNN_M.glsl')),
      );
      // 两档都少于精细档（精细 = 7 个文件，含两个 AutoDownscalePre）。
      expect(
        VideoEnhancementAssets.animeChainBalanced.length,
        lessThan(VideoEnhancementAssets.animeChainFine.length),
      );
    });

    // [极简超分档·+143] 与极简档同价（都是 2 文件、972 MAC/px）但把算力
    // 花在 2× 超分上：Upscale_Denoise_S 是官方 Mode C 开链组件（无 Restore
    // 链必须以 Upscale_Denoise/Denoise 开头），其 HOOK/WHEN/SAVE 与
    // Upscale_S 逐字一致，固定表面下同样产出 Depth-to-Space，_monitor
    // 的 expectsUpscale（_appliedTier != minimal）天然为 true。
    test('极简超分档是 Clamp + Denoise 版超分，无 Restore 但与极简档同价', () {
      expect(VideoEnhancementAssets.animeChainMinimalUpscale, [
        'Anime4K_Clamp_Highlights.glsl',
        'Anime4K_Upscale_Denoise_CNN_x2_S.glsl',
      ]);
      // 与极简档同为 2 文件；不含任何 Restore，也不含普通版 Upscale_S。
      expect(
        VideoEnhancementAssets.animeChainMinimalUpscale.length,
        VideoEnhancementAssets.animeChainMinimal.length,
      );
      expect(
        VideoEnhancementAssets.animeChainMinimalUpscale,
        isNot(contains('Anime4K_Restore_CNN_S.glsl')),
      );
      expect(
        VideoEnhancementAssets.animeChainMinimalUpscale,
        isNot(contains('Anime4K_Upscale_CNN_x2_S.glsl')),
      );
      // 比均衡档少一个 Restore_S 文件（超分网络内置降噪，无需 Restore 兜底）。
      expect(
        VideoEnhancementAssets.animeChainMinimalUpscale.length,
        lessThan(VideoEnhancementAssets.animeChainBalanced.length),
      );
    });

    test('均衡/标准/极简超分档往返序列化保持', () {
      for (final tier in const [
        VideoEnhancementTier.balanced,
        VideoEnhancementTier.standard,
        VideoEnhancementTier.minimalUpscale,
      ]) {
        final preferences = VideoEnhancementPreferences(
          mode: VideoEnhancementMode.advanced,
          tier: tier,
        );
        expect(
          VideoEnhancementPreferences.fromJson(preferences.toJson()).tier,
          tier,
        );
      }
    });

    test('档位声明顺序即成本升序：质量 > 轻量 > 标准 > 均衡 > 极简超分 > 极简', () {
      expect(VideoEnhancementTier.values, [
        VideoEnhancementTier.minimal,
        VideoEnhancementTier.minimalUpscale,
        VideoEnhancementTier.balanced,
        VideoEnhancementTier.standard,
        VideoEnhancementTier.fine,
        VideoEnhancementTier.quality,
      ]);
    });

    // [阶梯降档·+142] nextCheaperTier 是看门狗降档的唯一依据，其正确性
    // 直接决定「过载先降档而非直接放弃增强」是否按预期逐级下移。
    // [极简超分档·+143] 均衡档的下一站是极简超分档而非极简档——同价但
    // 保留 2× 超分；极简超分档过载再降到极简档（丢失超分换 1 个 pass）。
    test('nextCheaperTier 逐级下移，极简返回 null', () {
      expect(
        nextCheaperTier(VideoEnhancementTier.quality),
        VideoEnhancementTier.fine,
      );
      expect(
        nextCheaperTier(VideoEnhancementTier.fine),
        VideoEnhancementTier.standard,
      );
      expect(
        nextCheaperTier(VideoEnhancementTier.standard),
        VideoEnhancementTier.balanced,
      );
      expect(
        nextCheaperTier(VideoEnhancementTier.balanced),
        VideoEnhancementTier.minimalUpscale,
      );
      expect(
        nextCheaperTier(VideoEnhancementTier.minimalUpscale),
        VideoEnhancementTier.minimal,
      );
      expect(nextCheaperTier(VideoEnhancementTier.minimal), isNull);
    });

    test('极简档往返序列化保持，且不影响 mode/content', () {
      const preferences = VideoEnhancementPreferences(
        mode: VideoEnhancementMode.advanced,
        content: VideoEnhancementContent.animation,
        tier: VideoEnhancementTier.minimal,
      );
      expect(
        VideoEnhancementPreferences.fromJson(preferences.toJson()),
        preferences,
      );
      final switched = preferences.copyWith(tier: VideoEnhancementTier.fine);
      expect(switched.mode, VideoEnhancementMode.advanced);
      expect(switched.content, VideoEnhancementContent.animation);
    });

    test('quality 往返序列化保持', () {
      const preferences = VideoEnhancementPreferences(
        mode: VideoEnhancementMode.advanced,
        tier: VideoEnhancementTier.quality,
      );
      expect(
        VideoEnhancementPreferences.fromJson(preferences.toJson()),
        preferences,
      );
    });

    test('旧 JSON 无 tier 键回落 balanced，非法值同样回落', () {
      expect(
        VideoEnhancementPreferences.fromJson({'mode': 'advanced'}).tier,
        VideoEnhancementTier.balanced,
      );
      expect(
        VideoEnhancementPreferences.fromJson({
          'mode': 'advanced',
          'tier': 'garbage',
        }).tier,
        VideoEnhancementTier.balanced,
      );
    });

    // [+144 更名迁移] lite 更名为 fine：显式存过 'lite' 的老用户必须继续
    // 映射到精细档，而不是掉回落 Default（balanced）。
    test("旧 JSON 的 'lite' 经别名迁移到 fine", () {
      expect(
        VideoEnhancementPreferences.fromJson({
          'mode': 'advanced',
          'tier': 'lite',
        }).tier,
        VideoEnhancementTier.fine,
      );
    });

    test('copyWith 切档位不影响 mode/content', () {
      const preferences = VideoEnhancementPreferences(
        mode: VideoEnhancementMode.advanced,
        content: VideoEnhancementContent.animation,
      );
      final switched = preferences.copyWith(
        tier: VideoEnhancementTier.quality,
      );
      expect(switched.mode, VideoEnhancementMode.advanced);
      expect(switched.content, VideoEnhancementContent.animation);
      expect(switched.tier, VideoEnhancementTier.quality);
      expect(switched, isNot(preferences));
    });
  });

  /// 固定输出表面（VideoFixedSurface，2026-10 落地）后的策略语义。
  ///
  /// 背景：安卓端默认表面恒为源尺寸，Anime4K 的 Upscale pass 因 WHEN 条件被
  /// mpv 跳过。启用固定表面后，fork 在首次 videoParams 事件上把表面一次性放大
  /// 到「源 × 倍率」并锁定，策略层拿到的 surfaceOutput 即该实际表面——
  /// 这里固定「output 恒为传入的 fixedSurface、与视口/平台无关」的契约，
  /// 它是 video_enhancement.dart _monitor 的 sizeMatches / expectsUpscale
  /// 不误拉黑后端的前提。
  group('固定表面（fixedSurface）语义', () {
    final source = params(width: 544, height: 960);

    test('安卓：output 恒为固定表面，半屏/全屏一致', () {
      const surface = Size(1088, 1920); // 544×960 × 2
      for (final viewport in const [Size(1080, 1344), Size(1080, 2400)]) {
        final decision = decide(
          parameters: source,
          viewport: viewport,
          android: true,
          fixedSurface: surface,
        );
        expect(decision.backend, VideoEnhancementBackend.anime);
        expect(decision.output, surface);
      }
    });

    test('固定表面=源尺寸时退回默认语义（同分辨率重建）', () {
      const surface = Size(544, 960);
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
        fixedSurface: surface,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, surface);
    });

    test('固定表面过大撑爆显存预算时回落 scaling（预算含表面项）', () {
      // 890×1600 源 + 2560×1440 表面：神经项 ~280MB、ravu 项 ~193MB，
      // 均超安卓 128MB 预算，与视口无关地回落到高质量缩放。
      const surface = Size(2560, 1440);
      final decision = decide(
        parameters: params(width: 890, height: 1600),
        viewport: const Size(1080, 2400),
        android: true,
        fixedSurface: surface,
      );
      expect(decision.backend, VideoEnhancementBackend.scaling);
      expect(decision.output, surface);
    });

    test('桌面端同样以 fixedSurface 为 output（策略与平台无关）', () {
      const surface = Size(1088, 1920);
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: false,
        fixedSurface: surface,
      );
      expect(decision.backend, VideoEnhancementBackend.anime);
      expect(decision.output, surface);
    });

    test('增强关闭时固定表面不生效', () {
      const surface = Size(1088, 1920);
      final decision = decide(
        parameters: source,
        viewport: const Size(1080, 1344),
        android: true,
        preferences: const VideoEnhancementPreferences(
          mode: VideoEnhancementMode.off,
        ),
        fixedSurface: surface,
      );
      expect(decision.backend, VideoEnhancementBackend.original);
      expect(decision.output, null);
    });
  });
}
