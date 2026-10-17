import 'video_enhancement_preferences.dart';

const playbackSpeeds = [.5, .75, 1.0, 1.25, 1.5, 2.0, 3.0];

/// 按住画面时临时使用的倍速档位（松开即回到 [PlaybackPreferences.speed]）。
const holdSpeeds = [1.5, 1.75, 2.0, 2.5, 3.0, 4.0, 5.0];
const defaultHoldSpeed = 3.0;

const danmakuScales = [(.75, '小'), (1.0, '标准'), (1.25, '大'), (1.5, '特大')];
const danmakuOpacities = [.25, .5, .75, 1.0];

class PlaybackPreferences {
  const PlaybackPreferences({
    this.speed = 1,
    this.holdSpeed = defaultHoldSpeed,
    this.quality = 0,
    this.autoAdvance = true,
    this.danmaku = true,
    this.danmakuScale = 1,
    this.danmakuOpacity = 1,
    this.preload = true,
    this.volume = 100,
    this.enhancement = const VideoEnhancementPreferences(),
  });

  final double speed;
  final double holdSpeed;
  final int quality;
  final bool autoAdvance;
  final bool danmaku;
  final double danmakuScale;
  final double danmakuOpacity;
  final bool preload;
  final double volume;
  final VideoEnhancementPreferences enhancement;

  PlaybackPreferences copyWith({
    double? speed,
    double? holdSpeed,
    int? quality,
    bool? autoAdvance,
    bool? danmaku,
    double? danmakuScale,
    double? danmakuOpacity,
    bool? preload,
    double? volume,
    VideoEnhancementPreferences? enhancement,
  }) => PlaybackPreferences(
    speed: speed ?? this.speed,
    holdSpeed: holdSpeed ?? this.holdSpeed,
    quality: quality ?? this.quality,
    autoAdvance: autoAdvance ?? this.autoAdvance,
    danmaku: danmaku ?? this.danmaku,
    danmakuScale: danmakuScale ?? this.danmakuScale,
    danmakuOpacity: danmakuOpacity ?? this.danmakuOpacity,
    preload: preload ?? this.preload,
    volume: volume ?? this.volume,
    enhancement: enhancement ?? this.enhancement,
  );

  Map<String, dynamic> toJson() => {
    'speed': speed,
    'holdSpeed': holdSpeed,
    'quality': quality,
    'autoAdvance': autoAdvance,
    'danmaku': danmaku,
    'danmakuScale': danmakuScale,
    'danmakuOpacity': danmakuOpacity,
    'preload': preload,
    'volume': volume,
    'enhancement': enhancement.toJson(),
  };

  factory PlaybackPreferences.fromJson(Map<String, dynamic> value) {
    final speed = (value['speed'] as num? ?? 1).toDouble();
    // 旧版本没有长按倍速键（2026-10 起可设置），缺省或异常取值都回落到
    // 原来的固定 3 倍，不因此让整份播放偏好失效。
    final savedHoldSpeed = (value['holdSpeed'] as num?)?.toDouble();
    final holdSpeed = holdSpeeds.contains(savedHoldSpeed)
        ? savedHoldSpeed!
        : defaultHoldSpeed;
    final quality = value['quality'] as int? ?? 0;
    final autoAdvance = value['autoAdvance'] as bool? ?? true;
    final danmaku = value['danmaku'] as bool? ?? true;
    final savedDanmakuScale = (value['danmakuScale'] as num?)?.toDouble();
    final danmakuScale =
        danmakuScales.any((scale) => scale.$1 == savedDanmakuScale)
        ? savedDanmakuScale!
        : 1.0;
    final savedDanmakuOpacity = (value['danmakuOpacity'] as num?)?.toDouble();
    final danmakuOpacity = danmakuOpacities.contains(savedDanmakuOpacity)
        ? savedDanmakuOpacity!
        : 1.0;
    final preload = value['preload'] as bool? ?? true;
    final savedVolume = (value['volume'] as num?)?.toDouble() ?? 100;
    final volume = savedVolume.isFinite
        ? savedVolume.clamp(0.0, 100.0).toDouble()
        : 100.0;
    if (!playbackSpeeds.contains(speed) || quality < 0 || quality > 4320) {
      throw const FormatException('播放偏好无效');
    }
    return PlaybackPreferences(
      speed: speed,
      holdSpeed: holdSpeed,
      quality: quality,
      autoAdvance: autoAdvance,
      danmaku: danmaku,
      danmakuScale: danmakuScale,
      danmakuOpacity: danmakuOpacity,
      preload: preload,
      volume: volume,
      enhancement: VideoEnhancementPreferences.fromJson(value['enhancement']),
    );
  }
}
