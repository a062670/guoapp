import 'dart:math' as math;

import 'package:flutter/material.dart';

/// 片源清晰度自动检测 + 进度条节点 UI。
///
/// ## 检测来源
/// 播放器解码后通过 `PlayerStream.videoParams`（mpv 的 w/h/dw/dh）上报真实
/// 片源像素尺寸，[resolutionLevelOf] 把高度折算成常见档位。没有拿到尺寸
/// （未起播 / 音频 / 检测失败）时显示「待检测」提示，进度条停在 0，不猜测档位。
///
/// ## 节点与填充
/// 进度条上有 4 个节点（480P / 1080P / 2K / 4K），按高度对数刻度定位：
/// 填充比例 = ln(检测高度/360) / ln(2160/360)，夹在 [0,1]。用对数而不是
/// 线性，是为了让 1080P 落在条中部而不是贴着左端，节点间距在视觉上更均匀。
class ResolutionLevel {
  const ResolutionLevel(this.label, this.minHeight, this.major)
      : assert(minHeight > 0);

  /// 节点文案，如「1080P」「2K」。
  final String label;

  /// 到达该档位所需的最小视频高度（像素）。
  final int minHeight;

  /// 主节点画大圆点（1080P / 2K / 4K），次节点（480P）画小圆点。
  final bool major;

  /// 对数刻度下的相对位置（0..1），以 360p 为底、2160p 为顶。
  double get position {
    const floorHeight = 360.0;
    const ceilHeight = 2160.0;
    final clamped = minHeight.clamp(floorHeight, ceilHeight).toDouble();
    return _logScale(clamped, floorHeight, ceilHeight);
  }
}

/// 清晰度节点组：480P（次）/ 1080P / 2K / 4K（主）。
const resolutionLevels = <ResolutionLevel>[
  ResolutionLevel('480P', 480, false),
  ResolutionLevel('1080P', 1080, true),
  ResolutionLevel('2K', 1440, true),
  ResolutionLevel('4K', 2160, true),
];

/// 与 [resolutionLevels] 配套的节点组件 Key（ValueKey('resolution-node-4K')）。
/// UI 测试用它定位节点，避免和清晰度 chip 的文案文本混淆。
Key resolutionNodeKey(String label) => ValueKey('resolution-node-$label');

double _logScale(double value, double min, double max) {
  final denominator = math.log(max / min);
  if (denominator <= 0) return 0;
  return (math.log(value / min) / denominator).clamp(0.0, 1.0);
}

/// 折算高度对应的档位（取「不超过该高度的最大节点」）。
///
/// 高于 4K 的片源归入 4K 档；高度无效（<=0）返回 null，由
/// [ResolutionMeter] 的提示文案兜底，不猜测档位。
ResolutionLevel? resolutionLevelOf(int height) {
  if (height <= 0) return null;
  ResolutionLevel? matched;
  for (final level in resolutionLevels) {
    if (height >= level.minHeight) matched = level;
  }
  return matched;
}

/// 检测高度换算成填充比例（0..1）。height<=0 时为 0。
double resolutionProgressOf(int height) {
  if (height <= 0) return 0;
  return _logScale(height.clamp(360, 2160).toDouble(), 360, 2160);
}

/// 「已检测 1080P (1920×1080)」一类的说明行；未检测到尺寸时给提示语。
String resolutionCaption(int width, int height) {
  if (width <= 0 || height <= 0) {
    return '正在检测片源尺寸，起播后显示实际分辨率';
  }
  final level = resolutionLevelOf(height);
  final pixels = '$width×$height';
  return level == null ? '已检测 $pixels' : '已检测 ${level.label} ($pixels)';
}

/// 清晰度进度条：一条带节点刻度的进度条 + 节点标签行 + 检测说明。
///
/// 只做展示，无手势；播放进度条仍由播放器控制条负责，两者互不干扰。
class ResolutionMeter extends StatelessWidget {
  const ResolutionMeter({
    super.key,
    required this.width,
    required this.height,
    this.label,
    this.compact = false,
    this.showCaption = true,
  });

  /// 检测到的片源像素宽；<=0 表示尚未检测到。
  final int width;

  /// 检测到的片源像素高；<=0 表示尚未检测到。
  final int height;

  /// 进度条上方小标签（如「清晰度」）；空则不显示。
  final String? label;

  /// 紧凑模式（菜单 / 控制条用）：节点文字更小、整体更矮。
  final bool compact;

  /// 是否显示底部的「已检测 1080P (1920×1080)」说明行。
  /// 窄屏控制条可关掉省高度；无障碍语义仍包含该说明。
  final bool showCaption;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final detected = width > 0 && height > 0;
    final progress = resolutionProgressOf(height);
    final level = resolutionLevelOf(height);
    final textStyle = theme.textTheme.labelSmall?.copyWith(
      color: Colors.white70,
      letterSpacing: .3,
    );
    final activeColor = theme.colorScheme.primary;
    const trackHeight = 6.0;
    final barHeight = compact ? 18.0 : 22.0;
    // 「1080P」在 11px labelSmall 下约 35px，再留大字号缩放余量；
    // 相邻节点间距（2K↔4K 最小约 68px@312 宽）足够放下 48px 标签。
    final labelWidth = compact ? 40.0 : 48.0;

    return Semantics(
      label: resolutionCaption(width, height),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          if (label != null && label!.isNotEmpty) ...[
            Text(label!, style: textStyle),
            const SizedBox(height: 6),
          ],
          SizedBox(
            height: barHeight,
            child: LayoutBuilder(
              builder: (context, constraints) {
                final totalWidth = constraints.maxWidth;
                return Stack(
                  clipBehavior: Clip.none,
                  alignment: Alignment.centerLeft,
                  children: [
                    // 底槽
                    Positioned(
                      left: 0,
                      right: 0,
                      top: barHeight / 2 - trackHeight / 2,
                      child: Container(
                        height: trackHeight,
                        decoration: BoxDecoration(
                          color: Colors.white24,
                          borderRadius: BorderRadius.circular(trackHeight / 2),
                        ),
                      ),
                    ),
                    // 填充段（按检测分辨率），用对齐容器手画而不是
                    // FractionallySizedBox，避免 Stack 内部约束歧义。
                    Align(
                      alignment: Alignment.centerLeft,
                      child: AnimatedContainer(
                        duration: const Duration(milliseconds: 320),
                        curve: Curves.easeOutCubic,
                        width: totalWidth * progress,
                        height: trackHeight,
                        decoration: BoxDecoration(
                          color: activeColor,
                          borderRadius: BorderRadius.circular(trackHeight / 2),
                        ),
                      ),
                    ),
                    // 节点圆点
                    for (final node in resolutionLevels)
                      Positioned(
                        key: resolutionNodeKey(node.label),
                        left: totalWidth * node.position - (node.major ? 5 : 3),
                        top: barHeight / 2 - (node.major ? 5 : 3),
                        child: Container(
                          width: node.major ? 10 : 6,
                          height: node.major ? 10 : 6,
                          decoration: BoxDecoration(
                            color: !detected
                                ? Colors.white54
                                : level == node
                                    ? Colors.white
                                    : height >= node.minHeight
                                        ? activeColor
                                        : Colors.white54,
                            shape: BoxShape.circle,
                            border: Border.all(
                              color: Colors.black.withValues(alpha: .35),
                            ),
                          ),
                        ),
                      ),
                  ],
                );
              },
            ),
          ),
          const SizedBox(height: 6),
          // 节点标签行：按刻度定位，右侧节点做右对齐防溢出。
          SizedBox(
            height: compact ? 14 : 16,
            child: LayoutBuilder(
              builder: (context, constraints) {
                final totalWidth = constraints.maxWidth;
                return Stack(
                  clipBehavior: Clip.none,
                  children: [
                    for (final node in resolutionLevels)
                      Positioned(
                        key: resolutionNodeKey('${node.label}-label'),
                        left: _labelLeft(
                          totalWidth * node.position,
                          labelWidth,
                          totalWidth,
                        ),
                        child: SizedBox(
                          width: labelWidth,
                          // 标签绝不换行：「480P」「1080P」必须单行渲染，
                          // 否则小屏 / 大字号下会被挤成两行（P 掉到下一行）。
                          child: Text(
                            node.label,
                            textAlign: TextAlign.center,
                            maxLines: 1,
                            softWrap: false,
                            overflow: TextOverflow.visible,
                            style: textStyle?.copyWith(
                              fontWeight:
                                  detected && height >= node.minHeight
                                      ? FontWeight.w700
                                      : FontWeight.w400,
                              color: detected && height >= node.minHeight
                                  ? Colors.white
                                  : Colors.white54,
                            ),
                          ),
                        ),
                      ),
                  ],
                );
              },
            ),
          ),
          const SizedBox(height: 4),
          if (showCaption)
            Text(
              resolutionCaption(width, height),
              style: theme.textTheme.bodySmall?.copyWith(color: Colors.white70),
            ),
        ],
      ),
    );
  }
}

/// 标签定位：刻度中心对齐标签中心，靠边时夹住避免溢出。
double _labelLeft(double center, double labelWidth, double totalWidth) {
  final left = center - labelWidth / 2;
  if (left < 0) return 0;
  if (left + labelWidth > totalWidth) return totalWidth - labelWidth;
  return left;
}
