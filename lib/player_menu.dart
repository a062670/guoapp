import 'dart:math' as math;

import 'package:flutter/material.dart';

import 'models.dart';
import 'episode_browser.dart';
import 'playback_preferences.dart';
import 'resolution_meter.dart';
import 'video_enhancement.dart';
import 'video_enhancement_settings.dart';
import 'diary_service.dart';

enum PlayerMenuSection { episodes, speed, quality, settings }

/// 播放线路选项：LibreTV 等片源的同一集可能来自多个播放源，
/// 在详情里表现为「第N集」「第N集·源名」的并列章节。
class PlayerLine {
  const PlayerLine({required this.index, required this.label});

  /// 在 episodes 列表里的下标（切换线路 = 切到该章节）。
  final int index;
  final String label;
}

class PlayerMenu extends StatefulWidget {
  const PlayerMenu({
    super.key,
    required this.section,
    required this.episodes,
    required this.currentIndex,
    required this.preferences,
    required this.qualities,
    required this.actualQuality,
    required this.local,
    required this.favorite,
    required this.mobile,
    required this.onEpisode,
    required this.onPreferences,
    required this.onFavorite,
    this.videoWidth = 0,
    this.videoHeight = 0,
    this.lines = const [],
    this.showDanmaku = false,
    this.danmakuStatus = '',
    this.onRetryDanmaku,
    this.preloadStatus = '',
    this.enhancement,
    this.onCompareEnhancement,
  });

  final PlayerMenuSection section;
  final List<Episode> episodes;
  final int currentIndex;
  final PlaybackPreferences preferences;
  final List<int> qualities;
  final int actualQuality;

  /// 自动检测到的片源像素宽/高（0 = 尚未检测到）。
  final int videoWidth;
  final int videoHeight;
  final bool local;
  final bool favorite;
  final bool mobile;

  /// 同集的可选线路（含当前线路）；空表示该源没有多线路信息。
  final List<PlayerLine> lines;
  final bool showDanmaku;
  final String danmakuStatus;
  final VoidCallback? onRetryDanmaku;
  final String preloadStatus;
  final VideoEnhancementController? enhancement;
  final VoidCallback? onCompareEnhancement;
  final ValueChanged<int> onEpisode;
  final Future<void> Function(PlaybackPreferences) onPreferences;
  final Future<void> Function() onFavorite;

  @override
  State<PlayerMenu> createState() => _PlayerMenuState();
}

class _PlayerMenuState extends State<PlayerMenu> {
  bool _busy = false;
  String? _error;

  Future<void> _run(Future<void> Function() action) async {
    if (_busy) return;
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      await action();
    } catch (_) {
      if (mounted) setState(() => _error = '未能保存设置，请检查存储空间后重试。');
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final size = MediaQuery.sizeOf(context);
    final landscape = size.width > size.height;
    final colors = Theme.of(context).colorScheme;
    final hasQualities = widget.qualities.any((quality) => quality > 0);
    final hasLines = widget.lines.length > 1;
    final title = switch (widget.section) {
      PlayerMenuSection.episodes => '选集 · 共 ${widget.episodes.length} 集',
      PlayerMenuSection.speed => '播放倍速',
      // 没有真实清晰度档位（如 LibreTV 采集源）时，这个入口实际提供的是线路选择。
      PlayerMenuSection.quality =>
        hasLines ? (hasQualities ? '清晰度 / 线路' : '线路') : '清晰度',
      PlayerMenuSection.settings => '播放设置',
    };
    return Dialog(
      key: const ValueKey('player-menu'),
      alignment: landscape ? Alignment.centerRight : Alignment.bottomCenter,
      insetPadding: const EdgeInsets.all(12),
      backgroundColor: colors.surface,
      child: SizedBox(
        width: landscape ? math.min(440, size.width * .6) : 600,
        height: landscape ? size.height : size.height * .72,
        child: Column(
          children: [
            Padding(
              padding: const EdgeInsets.fromLTRB(18, 8, 4, 4),
              child: Row(
                children: [
                  Expanded(
                    child: Text(
                      title,
                      style: Theme.of(context).textTheme.titleMedium,
                    ),
                  ),
                  IconButton(
                    tooltip: '关闭菜单',
                    onPressed: () => Navigator.pop(context),
                    icon: const Icon(Icons.close_rounded),
                  ),
                ],
              ),
            ),
            if (_busy) const LinearProgressIndicator(minHeight: 2),
            if (_error != null)
              Padding(
                padding: const EdgeInsets.symmetric(
                  horizontal: 18,
                  vertical: 8,
                ),
                child: Text(
                  _error!,
                  style: const TextStyle(color: Color(0xFFFFB4A8)),
                ),
              ),
            Expanded(
              child: widget.section == PlayerMenuSection.episodes
                  ? PlayerEpisodeGrid(
                      episodes: widget.episodes,
                      currentIndex: widget.currentIndex,
                      keyPrefix: 'menu-episode',
                      onSelected: widget.onEpisode,
                    )
                  : SingleChildScrollView(
                      padding: const EdgeInsets.fromLTRB(18, 4, 18, 20),
                      child: _settings(),
                    ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _settings() {
    final preferences = widget.preferences;
    final all = widget.section == PlayerMenuSection.settings;
    final colors = Theme.of(context).colorScheme;
    final helperStyle = TextStyle(
      fontSize: 13,
      color: colors.onSurfaceVariant,
      height: 1.5,
    );
    final qualities = {
      0,
      ...widget.qualities.where((quality) => quality > 0),
    }.toList();
    final hasQualities = widget.qualities.any((quality) => quality > 0);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (all || widget.section == PlayerMenuSection.speed) ...[
          const Text('倍速'),
          const SizedBox(height: 8),
          Wrap(
            spacing: 8,
            runSpacing: 8,
            children: [
              for (final speed in playbackSpeeds)
                ChoiceChip(
                  key: ValueKey('menu-speed-$speed'),
                  label: Text('${speed}x'),
                  selected: speed == preferences.speed,
                  onSelected: _busy
                      ? null
                      : (_) => _run(
                          () => widget.onPreferences(
                            preferences.copyWith(speed: speed),
                          ),
                        ),
                ),
            ],
          ),
          const SizedBox(height: 20),
          const Text('长按倍速'),
          const SizedBox(height: 4),
          Text(
            '按住画面时临时加速到这个倍速，松开自动恢复 ${preferences.speed}x。',
            style: helperStyle,
          ),
          const SizedBox(height: 8),
          Wrap(
            spacing: 8,
            runSpacing: 8,
            children: [
              for (final speed in holdSpeeds)
                ChoiceChip(
                  key: ValueKey('menu-hold-speed-$speed'),
                  label: Text('${speed}x'),
                  selected: speed == preferences.holdSpeed,
                  onSelected: _busy
                      ? null
                      : (_) => _run(
                          () => widget.onPreferences(
                            preferences.copyWith(holdSpeed: speed),
                          ),
                        ),
                ),
            ],
          ),
          const SizedBox(height: 20),
        ],
        if (all || widget.section == PlayerMenuSection.quality) ...[
          // 片源清晰度自动检测：进度条 + 节点（480P/1080P/2K/4K），
          // 按播放器上报的真实像素尺寸填充进度，本地/在线片源一视同仁。
          const Text('片源清晰度（自动检测）'),
          const SizedBox(height: 8),
          ResolutionMeter(
            width: widget.videoWidth,
            height: widget.videoHeight,
          ),
          const SizedBox(height: 20),
          // 线路：同一集来自多个播放源时可手动切换，避免只能靠自动回退。
          if (widget.lines.length > 1) ...[
            const Text('线路'),
            const SizedBox(height: 4),
            Text('同一集的其他播放源，切换后从该源重新取流；当前线路不可用时也会自动回退。', style: helperStyle),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final line in widget.lines)
                  ChoiceChip(
                    key: ValueKey('menu-line-${line.index}'),
                    label: Text(line.label),
                    selected: line.index == widget.currentIndex,
                    onSelected: _busy
                        ? null
                        : (_) => widget.onEpisode(line.index),
                  ),
              ],
            ),
            const SizedBox(height: 20),
          ],
          // 没有真实清晰度档位的源（如 LibreTV 采集站）不再显示只有一个
          // 「自动」选项的空区块，避免用户看到「清晰度是空的」。
          if (hasQualities) ...[
            const Text('清晰度'),
            const SizedBox(height: 8),
            Text(
              widget.local
                  ? '本集正在播放本地原画，画质偏好用于后续在线播放。'
                  : widget.actualQuality > 0
                  ? '当前播放 ${widget.actualQuality}P'
                  : '使用源站可用画质',
              style: helperStyle,
            ),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final quality in qualities)
                  ChoiceChip(
                    key: ValueKey('menu-quality-$quality'),
                    label: Text(quality == 0 ? '自动（优先高清）' : '${quality}P'),
                    selected: quality == preferences.quality,
                    onSelected: _busy
                        ? null
                        : (_) => _run(
                            () => widget.onPreferences(
                              preferences.copyWith(quality: quality),
                            ),
                          ),
                  ),
              ],
            ),
            if (!qualities.contains(preferences.quality))
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  '已保存 ${preferences.quality}P 偏好，本集暂无此画质。',
                  style: helperStyle,
                ),
              ),
            const SizedBox(height: 20),
          ],
          // 既没有画质档位也没有多线路（单线路采集源）：给一句解释，
          // 不留一个空白面板。
          if (!hasQualities && widget.lines.length <= 1)
            Text(
              widget.local
                  ? '本集正在播放本地原画，没有可切换的网络清晰度。'
                  : '当前片源没有可选的清晰度或线路，由站源自动选择播放地址。',
              style: helperStyle,
            ),
        ],
        if ((all || widget.section == PlayerMenuSection.quality) &&
            widget.enhancement != null)
          VideoEnhancementSettings(
            controller: widget.enhancement!,
            busy: _busy,
            onChanged: (enhancement) => _run(
              () => widget.onPreferences(
                preferences.copyWith(enhancement: enhancement),
              ),
            ),
            onCompare: widget.onCompareEnhancement ?? () {},
          ),
        if (all && widget.showDanmaku) ...[
          SwitchListTile.adaptive(
            key: const ValueKey('player-danmaku-enabled'),
            contentPadding: EdgeInsets.zero,
            title: const Text('弹幕'),
            subtitle: Text(widget.danmakuStatus),
            value: preferences.danmaku,
            onChanged: _busy
                ? null
                : (value) => _run(
                    () => widget.onPreferences(
                      preferences.copyWith(danmaku: value),
                    ),
                  ),
          ),
          if (widget.onRetryDanmaku != null)
            TextButton.icon(
              key: const ValueKey('player-danmaku-retry'),
              onPressed: _busy ? null : widget.onRetryDanmaku,
              icon: const Icon(Icons.refresh_rounded),
              label: const Text('重试弹幕'),
            ),
          if (preferences.danmaku) ...[
            const SizedBox(height: 8),
            const Text('弹幕大小'),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final (scale, label) in danmakuScales)
                  ChoiceChip(
                    key: ValueKey('menu-danmaku-scale-$scale'),
                    label: Text(label),
                    selected: scale == preferences.danmakuScale,
                    onSelected: _busy
                        ? null
                        : (_) => _run(
                            () => widget.onPreferences(
                              preferences.copyWith(danmakuScale: scale),
                            ),
                          ),
                  ),
              ],
            ),
            const SizedBox(height: 16),
            const Text('弹幕透明度'),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              runSpacing: 8,
              children: [
                for (final opacity in danmakuOpacities)
                  ChoiceChip(
                    key: ValueKey('menu-danmaku-opacity-$opacity'),
                    label: Text('${(opacity * 100).round()}%'),
                    selected: opacity == preferences.danmakuOpacity,
                    onSelected: _busy
                        ? null
                        : (_) => _run(
                            () => widget.onPreferences(
                              preferences.copyWith(danmakuOpacity: opacity),
                            ),
                          ),
                  ),
              ],
            ),
          ],
          const SizedBox(height: 8),
        ],
        if (all) ...[
          SwitchListTile.adaptive(
            key: const ValueKey('player-preload-enabled'),
            contentPadding: EdgeInsets.zero,
            title: const Text('下一集预加载'),
            subtitle: Text(widget.local ? '本地播放不预取网络视频' : widget.preloadStatus),
            value: preferences.preload,
            onChanged: _busy
                ? null
                : (value) => _run(
                    () => widget.onPreferences(
                      preferences.copyWith(preload: value),
                    ),
                  ),
          ),
          const SizedBox(height: 8),
          SwitchListTile.adaptive(
            key: const ValueKey('player-auto-advance'),
            contentPadding: EdgeInsets.zero,
            title: const Text('自动连播'),
            subtitle: const Text('关闭后，播放结束停在当前集'),
            value: preferences.autoAdvance,
            onChanged: _busy
                ? null
                : (value) => _run(
                    () => widget.onPreferences(
                      preferences.copyWith(autoAdvance: value),
                    ),
                  ),
          ),
          const SizedBox(height: 8),
          OutlinedButton.icon(
            key: const ValueKey('player-favorite'),
            onPressed: _busy ? null : () => _run(widget.onFavorite),
            icon: Icon(
              widget.favorite
                  ? Icons.bookmark_rounded
                  : Icons.bookmark_border_rounded,
            ),
            label: Text(widget.favorite ? '取消追剧' : '加入追剧'),
          ),
          const SizedBox(height: 12),
          OutlinedButton.icon(
            key: const ValueKey('player-diary-btn'),
            onPressed: () => DiaryService.showDiaryDialog(context),
            icon: const Icon(Icons.receipt_long_rounded),
            label: const Text('查看播放调试日记'),
          ),
          const SizedBox(height: 20),
          Text(
            widget.mobile
                ? '上下滑切集；长按画面临时 3 倍速，松开恢复。竖屏轻点暂停，横屏轻点显示控制；双击播放或暂停。'
                : '空格：播放 / 暂停\n左右键：后退 / 快进 5 秒\n长按右键或画面：临时 3 倍速\n上下键：音量 ±5%，M：静音\nF、F11、Ctrl+F：全屏\nEsc：先关闭菜单，再退出全屏',
            style: helperStyle.copyWith(height: 1.6),
          ),
        ],
      ],
    );
  }
}

class PlayerEpisodeGrid extends StatelessWidget {
  const PlayerEpisodeGrid({
    super.key,
    required this.episodes,
    required this.currentIndex,
    required this.onSelected,
    this.keyPrefix = 'play-episode',
    this.compact = false,
    this.title = '选集',
  });
  final List<Episode> episodes;
  final int currentIndex;
  final ValueChanged<int> onSelected;
  final String keyPrefix;
  final bool compact;
  final String title;
  @override
  Widget build(BuildContext context) => EpisodeBrowser(
    episodes: episodes,
    currentNumber: episodes.isEmpty
        ? null
        : episodes[currentIndex.clamp(0, episodes.length - 1)].number,
    onSelected: onSelected,
    keyPrefix: keyPrefix,
    compact: compact,
    title: title,
  );
}
