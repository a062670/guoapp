import 'dart:io';

import 'package:flutter/material.dart';

import 'remote_widgets.dart';
import 'video_enhancement.dart';
import 'video_enhancement_preferences.dart';

class VideoEnhancementSettings extends StatelessWidget {
  const VideoEnhancementSettings({
    super.key,
    required this.controller,
    required this.onChanged,
    required this.onCompare,
    this.television = false,
    this.busy = false,
  });

  final VideoEnhancementController controller;
  final ValueChanged<VideoEnhancementPreferences> onChanged;
  final VoidCallback onCompare;
  final bool television;
  final bool busy;

  Widget _choice({
    required String id,
    required String label,
    required bool selected,
    required VoidCallback action,
    bool enabled = true,
  }) => television
      ? RemoteButton(
          key: ValueKey(id),
          label: label,
          selected: selected,
          onPressed: busy || !enabled ? null : action,
        )
      : ChoiceChip(
          key: ValueKey(id),
          label: Text(label),
          selected: selected,
          onSelected: busy || !enabled ? null : (_) => action(),
        );

  @override
  Widget build(BuildContext context) => AnimatedBuilder(
    animation: controller,
    builder: (context, _) {
      final preferences = controller.preferences;
      final colors = Theme.of(context).colorScheme;
      final helperStyle = TextStyle(
        fontSize: 13,
        color: colors.onSurfaceVariant,
        height: 1.5,
      );
      final mode = preferences.mode;
      final enabled = mode != VideoEnhancementMode.off;
      // [结构重构·2026-10] 顶层三档：关闭 / 自动（族：省电增强+清晰优先）/ 高级。
      // 「自动」是族标记，本身不对应枚举值；子档由下方「画质增强模式」chips 选择。
      final autoFamily =
          mode == VideoEnhancementMode.economy ||
          mode == VideoEnhancementMode.quality;
      final supported = controller.supported;
      return Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(
            '画质增强',
            style: television ? const TextStyle(fontSize: 18) : null,
          ),
          const SizedBox(height: 8),
          Wrap(
            spacing: 8,
            runSpacing: 8,
            children: [
              _choice(
                id: 'enhancement-mode-off',
                label: '关闭',
                selected: mode == VideoEnhancementMode.off,
                action: () => onChanged(
                  preferences.copyWith(mode: VideoEnhancementMode.off),
                ),
              ),
              _choice(
                id: 'enhancement-mode-auto',
                // [实验性标记·2026-10] 自动族按画面类型/设备状态猜后端，
                // 命中与效果稳定性都不如高级档确定，按产品要求标注实验性。
                label: '自动（实验性）',
                selected: autoFamily,
                enabled: supported,
                // 已在自动族内则保持当前子档；从关闭/高级切入时默认落省电增强
                // （与旧 automatic 用户的迁移默认一致，零行为意外）。
                action: () => onChanged(
                  preferences.copyWith(
                    mode: autoFamily ? mode : VideoEnhancementMode.economy,
                  ),
                ),
              ),
              _choice(
                id: 'enhancement-mode-advanced',
                label: '高级',
                selected: mode == VideoEnhancementMode.advanced,
                enabled: supported,
                action: () => onChanged(
                  preferences.copyWith(mode: VideoEnhancementMode.advanced),
                ),
              ),
            ],
          ),
          const SizedBox(height: 8),
          Text(_modeHelper(mode), style: helperStyle),
          if (mode == VideoEnhancementMode.advanced) ...[
            const SizedBox(height: 16),
            const Text('增强档位'),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final tier in VideoEnhancementTier.values)
                  _choice(
                    id: 'enhancement-tier-${tier.name}',
                    label: tier.label,
                    selected: preferences.tier == tier,
                    enabled: supported,
                    action: () => onChanged(preferences.copyWith(tier: tier)),
                  ),
              ],
            ),
            const SizedBox(height: 8),
            Text(
              switch (preferences.tier) {
                VideoEnhancementTier.quality =>
                  '质量档：VL 重建 + VL 超分，效果最好，但负载约为精细档 3.4 倍；'
                      '1080p 源在中端 GPU 上基本跑不动，卡顿时会自动逐级降档。',
                VideoEnhancementTier.fine =>
                  '精细档：M+S 重建 + M 超分，负载约为均衡档 2.6 倍；'
                      '画质优先，1080p 源请留意帧率。',
                VideoEnhancementTier.standard =>
                  '标准档：M 重建 + S 超分，负载约为均衡档 1.6 倍；'
                      '比均衡档重建更稳（低码率片源收益明显），比精细档省 40% 算力。',
                VideoEnhancementTier.balanced =>
                  '均衡档（默认）：S 重建 + S 超分，重压缩片源观感最稳的便宜档；'
                      '720p 源开固定表面恰好 2× 无损耗，1080p 源建议骁龙 8 系一类 GPU。',
                // [极简超分档·+143] 与极简档同价（972 MAC/px），但带 2× 超分；
                // 官方 Mode C 形态（Upscale_Denoise 开链、无 Restore），重压缩
                // 片源的 ringing 风险高于均衡档，靠 Denoise 训练 + Clamp 兜底。
                VideoEnhancementTier.minimalUpscale =>
                  '极简超分档：只留高光钳制 + Denoise 版超分，与极简档同价但给真 2× 超分；'
                      '为骁龙 865 一类跑不动均衡档的设备准备，重压缩片源请与均衡档对比观感。',
                VideoEnhancementTier.minimal =>
                  '极简档：只留高光钳制 + S 档重建，无超分链；'
                      '每帧 7 个 pass、负载最低，骁龙 865 一类设备也能跑。'
                      '同价位的极简超分档带 2× 超分，要超分请选那一档。',
              },
              style: helperStyle,
            ),
            // [阶梯降档·+142] 六档算力递增，看门狗按实际掉帧判定过载后
            // 逐级降档（质量→精细→标准→均衡→极简超分→极简），本集内自动执行、
            // 不改动用户选择；切档或换集后恢复。
            const SizedBox(height: 4),
            Text(
              '播放中若出现明显掉帧，增强会自动逐级降档（仅本集生效），'
              '降到极简仍卡才会回退高质量缩放。',
              style: helperStyle,
            ),
            // [固定输出表面·2026-10] 安卓端唯一的真超分路径：播放器创建时
            // 按片源一次性放大渲染目标（1080p 最高 2560×1440）并锁定，
            // Anime4K 的 Upscale 链才不会被 mpv 的 WHEN 条件跳过。桌面端
            // 走 setSize 动态放大，无需此说明。
            if (Platform.isAndroid) ...[
              const SizedBox(height: 8),
              Text(
                '安卓端固定输出表面：进入播放器时按片源分辨率一次性放大渲染目标'
                '（1080p 最高 2560×1440），超分链随之真正生效；表面锁定后不再'
                '变化，竖屏进入再旋转横屏不会重新放大，需要退出重进播放器。'
                '显存不足的设备仍会自动回退高质量缩放。',
                style: helperStyle,
              ),
            ],
          ],
          if (autoFamily) ...[
            const SizedBox(height: 16),
            const Text('画质增强模式'),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final level in const [
                  VideoEnhancementMode.economy,
                  VideoEnhancementMode.quality,
                ])
                  _choice(
                    id: 'enhancement-level-${level.name}',
                    label: level.label,
                    selected: mode == level,
                    enabled: supported,
                    action: () => onChanged(preferences.copyWith(mode: level)),
                  ),
              ],
            ),
            const SizedBox(height: 16),
            const Text('画面类型'),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final content in VideoEnhancementContent.values)
                  _choice(
                    id: 'enhancement-content-${content.name}',
                    label: content.label,
                    selected: preferences.content == content,
                    enabled: supported,
                    action: () =>
                        onChanged(preferences.copyWith(content: content)),
                  ),
              ],
            ),
            const SizedBox(height: 8),
            Text(
              preferences.content == VideoEnhancementContent.automatic
                  ? '${controller.contentLabel}；分类不准确时可手动切换。'
                  : '动漫档适用于动画和 AI 动漫；写实 AI 画面建议使用通用档。',
              style: helperStyle,
            ),
          ],
          if (enabled) ...[
            const SizedBox(height: 12),
            Text(controller.status, key: const ValueKey('enhancement-status')),
            if (controller.detail.isNotEmpty) ...[
              const SizedBox(height: 4),
              Text(controller.detail, style: helperStyle),
            ],
            const SizedBox(height: 8),
            if (television)
              RemoteButton(
                key: const ValueKey('enhancement-compare'),
                label: controller.comparing ? '恢复增强' : '原画对比',
                icon: Icons.compare_rounded,
                onPressed: controller.canCompare && !busy ? onCompare : null,
              )
            else
              OutlinedButton.icon(
                key: const ValueKey('enhancement-compare'),
                onPressed: controller.canCompare && !busy ? onCompare : null,
                icon: const Icon(Icons.compare_rounded),
                label: Text(controller.comparing ? '恢复增强' : '原画对比'),
              ),
          ] else if (!supported) ...[
            const SizedBox(height: 12),
            Text(controller.status, key: const ValueKey('enhancement-status')),
          ],
          const SizedBox(height: 20),
        ],
      );
    },
  );

  /// 顶层模式 chips 下方的说明文案。
  String _modeHelper(VideoEnhancementMode mode) => switch (mode) {
    VideoEnhancementMode.off => '默认关闭，不改变视频输出；开启后再配置画面类型和原画对比。',
    VideoEnhancementMode.economy || VideoEnhancementMode.quality =>
      '实验性：自动按画面和设备选择增强，命中与效果可能变化；省电使用轻量缩放，清晰优先允许更高负载。追求稳定效果请用高级档。建议先选择源站高清画质。',
    VideoEnhancementMode.advanced =>
      '高级模式直接强制 Anime4K 神经增强链（安卓端固定输出表面启用时为真超分、未启用为同分辨率重建/降噪，桌面端为 2× 超分），绕过全部自动判断（动画标签、温控、倍速、显存预算均不拦截）；渲染异常时仍会自动回退原画。下方可选五档，算力依次递增：极简 / 均衡 / 标准 / 轻量 / 质量，追求性价比选均衡或标准；极简档不含超分链，给不了真超分。',
  };
}
