import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:path/path.dart' as path;
import 'package:path_provider/path_provider.dart';

class VideoEnhancementAssets {
  static const root = 'assets/video_enhancement';
  static const ravuRgb = 'ravu-r2-rgb.hook';
  static const ravuLuma = 'ravu-lite-ar-r2.hook';
  static const fsrcnnx = 'FSRCNNX_x2_8-0-4-1.glsl';
  /// Anime4K 极简档链（+95：精细档的瘦身版，只留 S 档重建网络）。
  /// Clamp_Highlights 3 个 pass + Restore_CNN_S 4 个 pass，去掉精细档里的
  /// Restore_CNN_M（8 pass）与 Upscale 链；安卓端 Upscale 本就被 WHEN 跳过，
  /// 所以实际每帧从 15 个 pass 降到 7 个、卷积量约为原来的 30%，
  /// 骁龙 865（Adreno 650）一类 GPU 能跑，仍保留同分辨率重建带来的清晰感。
  static const animeChainMinimal = <String>[
    'Anime4K_Clamp_Highlights.glsl',
    'Anime4K_Restore_CNN_S.glsl',
  ];

  /// Anime4K 极简超分档链（+143：与极简档同价、但带真 2× 超分）。
  /// Clamp + Upscale_Denoise_S：官方 Mode C 的开链组合——定制规则明确
  /// 「不以 Restore 开头的链必须以 Upscale_Denoise 或 Denoise 开头，
  /// 因为几乎所有视频压缩算法都是有损的」。Denoise 版超分网络见过带压缩
  /// 伪影的输入，可独立承担片源，省掉 Restore_S（972 MAC/px）整段。
  /// 成本与极简档持平（972 MAC/px、65 采样/px、8 pass/帧 vs 极简 7 pass），
  /// 但预算花在 2× 超分而非同分辨率重建，是骁龙 865 一类跑不动均衡档
  /// （1944 MAC/px）的设备的真超分入门档。
  /// 边界（官方 Mode C 定位为低退化内容）：重压缩片源的 ringing 放大风险
  /// 高于有 Restore 兜底的均衡档，靠 Denoise 训练 + 链上 Clamp De-Ring
  /// 兜底；1080p→2560×1440 属 1.333× 场景，Upscale 仍按 2× 渲染再降回
  /// 表面（与均衡档同路径），720p→1440p 恰好 2× 无损耗。
  /// Upscale_Denoise_S 与 Upscale_S 的 HOOK/WHEN(1.2)/SAVE 名逐字一致，
  /// 属纯 drop-in 替换：_monitor 的 Depth-to-Space 判定、_isOwnPass 的
  /// Anime4K 匹配均无需改动（见 test/video_enhancement_policy_test.dart）。
  static const animeChainMinimalUpscale = <String>[
    'Anime4K_Clamp_Highlights.glsl',
    'Anime4K_Upscale_Denoise_CNN_x2_S.glsl',
  ];

  /// Anime4K 均衡档链（+142：最便宜的真超分组合，Clamp + Restore_S + Upscale_S）。
  /// 固定表面下 1080p → 2560×1440 属 1.333× 场景：Upscale 的卷积 pass 跑在
  /// 源分辨率（卷积合计 972 MAC/px，+143 脚本重算值，旧记 684 系口算偏差），
  /// 末尾 Depth-to-Space 按 2× 输出后再由 mpv
  /// 降回表面——M 档超分多出的容量大半在这一步降采样里丢掉，S 档以 47% 的
  /// 卷积量拿到接近观感。720p → 1440p 恰好 2×，无降采样损耗，是本档甜点。
  /// 12 个 pass（极简 7 / 精细 24），单像素 MAC 约为精细档的 38%。
  static const animeChainBalanced = <String>[
    'Anime4K_Clamp_Highlights.glsl',
    'Anime4K_Restore_CNN_S.glsl',
    'Anime4K_Upscale_CNN_x2_S.glsl',
  ];

  /// Anime4K 标准档链（+142：M 档重建 + S 档超分）。
  /// 与均衡档的差别在重建网络（M 1716 vs S 900 MAC/px）：对低码率/重压缩
  /// 片源，M 的 6 层 8 特征网络去伪影和线条重建明显更稳，代价是单像素 MAC
  /// 从 1944 涨到 3032（约为精细档 60%）。无 Restore_S 第二段重建——
  /// 精细档里那一段（972 MAC/px）在超分链前属可选增益，先砍它保帧率。
  static const animeChainStandard = <String>[
    'Anime4K_Clamp_Highlights.glsl',
    'Anime4K_Restore_CNN_M.glsl',
    'Anime4K_Upscale_CNN_x2_S.glsl',
  ];

  /// Anime4K 精细档链（+87 引入；+144 由 lite／轻量档更名为 fine／精细档，
  /// 常量名 animeChainLite 同步改为 animeChainFine，顺序即注入顺序）。
  /// Clamp_Highlights + Restore_CNN_M + Restore_CNN_S + Upscale_CNN_x2_M
  /// + AutoDownscalePre_x2/x4 + Upscale_CNN_x2_S。
  /// WHEN 全部保留：安卓端表面恒源尺寸，mpv 自动跳过 Upscale/AutoDownscalePre，
  /// 仅 Clamp + Restore 生效（同分辨率重建/降噪）；桌面端表面放大 2× 时全链生效。
  static const animeChainFine = <String>[
    'Anime4K_Clamp_Highlights.glsl',
    'Anime4K_Restore_CNN_M.glsl',
    'Anime4K_Restore_CNN_S.glsl',
    'Anime4K_Upscale_CNN_x2_M.glsl',
    'Anime4K_AutoDownscalePre_x2.glsl',
    'Anime4K_AutoDownscalePre_x4.glsl',
    'Anime4K_Upscale_CNN_x2_S.glsl',
  ];

  /// Anime4K 质量档链（+89，顺序即注入顺序）。
  /// Clamp_Highlights + Restore_CNN_VL + Upscale_CNN_x2_VL
  /// + AutoDownscalePre_x2/x4 + Upscale_CNN_x2_M。
  /// VL 变体层数多、权重文件大（各 ~145KB），负载显著高于精细档，
  /// 低端设备可能触发 video_enhancement.dart 的 _watchPerformance 自动降级。
  static const animeChainQuality = <String>[
    'Anime4K_Clamp_Highlights.glsl',
    'Anime4K_Restore_CNN_VL.glsl',
    'Anime4K_Upscale_CNN_x2_VL.glsl',
    'Anime4K_AutoDownscalePre_x2.glsl',
    'Anime4K_AutoDownscalePre_x4.glsl',
    'Anime4K_Upscale_CNN_x2_M.glsl',
  ];
  static final _pending = <String, Future<String>>{};

  static Future<String> prepare(String name) async {
    final existing = _pending[name];
    if (existing != null) return existing;
    final future = _prepare(name);
    _pending[name] = future;
    try {
      return await future;
    } finally {
      if (identical(_pending[name], future)) _pending.remove(name);
    }
  }

  static Future<String> _prepare(String name) async {
    if (!RegExp(r'^[A-Za-z0-9_.-]+$').hasMatch(name)) {
      throw const FormatException('增强资源名称无效');
    }
    final manifest =
        jsonDecode(await rootBundle.loadString('$root/manifest.json')) as Map;
    final entry = (manifest['shaders'] as Map)[name] as Map?;
    final digest = entry?['sha256'] as String? ?? '';
    final length = entry?['bytes'] as int? ?? 0;
    if (!RegExp(r'^[a-f0-9]{64}$').hasMatch(digest) ||
        length <= 0 ||
        length > 4 * 1024 * 1024) {
      throw const FormatException('增强资源清单无效');
    }
    final directory = Directory(
      path.join(
        (await getApplicationSupportDirectory()).path,
        'video_enhancement',
        'v1',
      ),
    );
    await directory.create(recursive: true);
    final destination = File(path.join(directory.path, '$digest-$name'));
    if (await destination.exists() && await destination.length() == length) {
      if (sha256.convert(await destination.readAsBytes()).toString() ==
          digest) {
        return destination.path;
      }
    }
    final data = await rootBundle.load('$root/$name');
    final bytes = data.buffer.asUint8List(
      data.offsetInBytes,
      data.lengthInBytes,
    );
    if (bytes.length != length || sha256.convert(bytes).toString() != digest) {
      throw const FormatException('增强资源校验失败');
    }
    final temporary = File(
      '${destination.path}.${DateTime.now().microsecondsSinceEpoch}.tmp',
    );
    File? previous;
    try {
      await temporary.writeAsBytes(bytes, flush: true);
      if (Platform.isWindows && await destination.exists()) {
        previous = await destination.rename('${temporary.path}.previous');
      }
      await temporary.rename(destination.path);
      if (previous != null && await previous.exists()) await previous.delete();
    } catch (_) {
      if (previous != null &&
          await previous.exists() &&
          !await destination.exists()) {
        await previous.rename(destination.path);
      }
      rethrow;
    } finally {
      if (await temporary.exists()) await temporary.delete();
    }
    return destination.path;
  }

  static void registerLicenses() {
    LicenseRegistry.addLicense(() async* {
      yield LicenseEntryWithLineBreaks(
        ['RAVU', 'FSRCNNX'],
        '${await rootBundle.loadString('$root/LGPL-3.0.txt')}\n\n'
        '${await rootBundle.loadString('$root/GPL-3.0.txt')}',
      );
      yield LicenseEntryWithLineBreaks([
        'Anime4K',
      ], await rootBundle.loadString('$root/Anime4K-LICENSE.txt'));
    });
  }
}
