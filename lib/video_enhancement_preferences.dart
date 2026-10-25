enum VideoEnhancementMode {
  off('关闭'),
  economy('省电增强'),
  quality('清晰优先'),
  advanced('高级');

  const VideoEnhancementMode(this.label);
  final String label;
}

enum VideoEnhancementContent {
  automatic('跟随分类'),
  general('通用／真人'),
  animation('动漫／AI 动漫');

  const VideoEnhancementContent(this.label);
  final String label;
}

/// 高级模式（advanced）下的 Anime4K 链档位。
///
/// 各档链定义见 video_enhancement_assets.dart；成本量级按 //!DESC 层规格折算
/// （Cout×Cin×k² 单像素 MAC，Clamp 约 20 忽略不计；+143 起数字为脚本重算值，
/// 旧注释的 920/1604/2420 系口算偏差）：
///   minimal        ≈ 972   Clamp + Restore_S（无超分）
///   minimalUpscale ≈ 972   Clamp + Upscale_Denoise_S（+143）
///   balanced       ≈ 1944  Clamp + Restore_S + Upscale_S（+142；+144 起默认档）
///   standard       ≈ 3032  Clamp + Restore_M + Upscale_S（+142）
///   fine           ≈ 5092  Clamp + Restore_M + Restore_S + Upscale_M
///                        （固定表面 1.333× 下两个 AutoDownscalePre 与末段
///                        Upscale_S 均被 WHEN 跳过；+144 前名 lite／轻量档）
///   quality        ≈ 17200 Clamp + Restore_VL + Upscale_VL（同上跳过规律）
/// [minimal] 极简档：只留高光钳制 + S 档重建，无超分链（+95）；
/// [minimalUpscale] 极简超分档：与极简档完全同价，但把算力从「同分辨率重建」
/// 挪到「2× 超分」——Clamp + Upscale_Denoise_S（官方 Mode C 的开链组件，
/// Denoise 训练版可独立承担压缩片源，无需 Restore 兜底）。为骁龙 865 一类
/// 跑不动均衡档的设备准备的真超分入门档（+143）；
/// [balanced] 均衡档：S 重建 + S 超分，重压缩片源观感最稳的便宜档——固定表面
/// 1.333× 场景下 Upscale 恒按 2× 渲染再降回目标，M 的额外容量大半被降采样丢掉，
/// S 以一半卷积量拿到接近观感；720p→1440p 恰好 2× 无损耗，是本档甜点（+142）；
/// +144 起为默认档（原默认 lite 对骁龙 865 已超载，见 settings 阶梯说明）；
/// [standard] 标准档：M 重建 + S 超分，重建质量优先但仍控算力（+142）；
/// [fine] 精细档：M+S 重建 + M 超分，负载约为均衡档 2.6 倍（+144 由 lite
/// 更名，标识符同步 lite→fine，旧持久化值走 _tierFromJson 别名迁移）；
/// [quality] 质量档：VL 重建 + VL 超分，效果优先，1080p 源在中端 GPU 上
/// 基本跑不动，仅建议 ≤720p 源或桌面端使用。
///
/// 声明顺序即算力升序——这同时是性能看门狗的阶梯降档顺序
/// （video_enhancement.dart 的 _watchPerformance 经 nextCheaperTier 逐级下移，
/// 到极简仍过载才放弃 anime 后端）。新增档位必须按成本插队，不能追加到末尾。
/// minimalUpscale 与 minimal 成本持平（972），声明在其后是为了让看门狗把
/// 过载的均衡档先降到「仍带超分」的极简超分档，而不是直接打回无超分的极简档。
enum VideoEnhancementTier {
  minimal('极简档'),
  minimalUpscale('极简超分档'),
  balanced('均衡档'),
  standard('标准档'),
  fine('精细档'),
  quality('质量档');

  const VideoEnhancementTier(this.label);
  final String label;
}

class VideoEnhancementPreferences {
  const VideoEnhancementPreferences({
    this.mode = VideoEnhancementMode.off,
    this.content = VideoEnhancementContent.automatic,
    this.tier = VideoEnhancementTier.balanced,
  });

  final VideoEnhancementMode mode;
  final VideoEnhancementContent content;
  final VideoEnhancementTier tier;

  VideoEnhancementPreferences copyWith({
    VideoEnhancementMode? mode,
    VideoEnhancementContent? content,
    VideoEnhancementTier? tier,
  }) => VideoEnhancementPreferences(
    mode: mode ?? this.mode,
    content: content ?? this.content,
    tier: tier ?? this.tier,
  );

  Map<String, dynamic> toJson() => {
    'mode': mode.name,
    'content': content.name,
    'tier': tier.name,
  };

  factory VideoEnhancementPreferences.fromJson(Object? value) {
    if (value is! Map) return const VideoEnhancementPreferences();
    return VideoEnhancementPreferences(
      mode: _modeFromJson(value['mode']),
      content:
          VideoEnhancementContent.values
              .where((content) => content.name == value['content'])
              .firstOrNull ??
          VideoEnhancementContent.automatic,
      tier: _tierFromJson(value['tier']),
    );
  }

  /// [结构迁移·2026-10] 旧版 mode 为扁平四档（off/automatic/economy/quality），
  /// 新版把省电增强/清晰优先收进「自动」族并新增「高级」强制档：
  ///   - 旧 'automatic'（安卓=不超分的保守自动）-> economy（省电增强），零行为变化；
  ///   - 旧 'economy'/'quality' 名称不变，直接对应；
  ///   - 新 'advanced' 为高级强制档；未知值（含旧版读新档）回落 off。
  /// [档位迁移·2026-10] tier 为 +89 新增字段：旧 JSON 无此键时回落当时默认的
  /// lite（与 +87 起的默认链一致，零行为意外）；非法值同样回落。
  /// [+144 更名] lite 更名为 fine（精细档）：标识符同步迁移，已持久化的
  /// 'lite' 经 _tierAliases 继续映射到 fine，老用户的选择不丢。
  /// [+144 默认档] 默认/回落值由 lite 改为 balanced——lite（现 fine）对骁龙
  /// 865 已超载（真机反馈），新装与旧版无偏好用户直接落在跑得动的均衡档；
  /// 注意这只影响「从未保存过 tier」的场景，显式存过 'lite' 的仍是精细档。
  static const Map<String, VideoEnhancementTier> _tierAliases = {
    'lite': VideoEnhancementTier.fine,
  };

  static VideoEnhancementTier _tierFromJson(Object? name) {
    final tier =
        VideoEnhancementTier.values
            .where((tier) => tier.name == name)
            .firstOrNull;
    return tier ?? _tierAliases[name] ?? VideoEnhancementTier.balanced;
  }

  static VideoEnhancementMode _modeFromJson(Object? name) => switch (name) {
    'economy' => VideoEnhancementMode.economy,
    'quality' => VideoEnhancementMode.quality,
    'advanced' => VideoEnhancementMode.advanced,
    'automatic' => VideoEnhancementMode.economy,
    _ => VideoEnhancementMode.off,
  };

  @override
  bool operator ==(Object other) =>
      other is VideoEnhancementPreferences &&
      other.mode == mode &&
      other.content == content &&
      other.tier == tier;

  @override
  int get hashCode => Object.hash(mode, content, tier);
}
