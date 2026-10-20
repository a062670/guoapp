import 'models.dart';

/// 一个已导入的 drpy 脚本片源（TVBox drpy2 / spider 规则，goja 运行时执行）。
/// 对应原生核心 drpy.json 里的一条站点记录，脚本正文在 scripts/&lt;key&gt;.js。
class DrpySite {
  const DrpySite({
    required this.key,
    required this.name,
    this.scriptUrl = '',
    this.title = '',
    this.host = '',
    this.count = 0,
    this.engine = '',
  });

  final String key;
  final String name;

  /// 导入来源（粘贴 JS 时为空）。
  final String scriptUrl;

  /// rule.title（静态提取，可能为空）。
  final String title;

  /// rule.host（静态提取，展示用）。
  final String host;

  /// 本机已缓存的剧集数（站源管理里展示）。
  final int count;

  /// 脚本方言：空 = drpy2，`catvod` = CatVod 蜘蛛（__jsEvalReturn）。
  final String engine;

  /// 是否 CatVod 蜘蛛（依赖 lib/cat.js，展示时加标记区分）。
  bool get isCatVod => engine == 'catvod';

  /// 完整源 ID，与 [SourceSite] 运行时注册表一致。
  String get sourceId => 'drpy:$key';

  /// 副标题：优先 rule.title，其次 host，最后「脚本源」。
  String get detailLabel {
    final title = this.title.trim();
    if (title.isNotEmpty) return title;
    final host = this.host.trim();
    if (host.isNotEmpty) return host;
    return '脚本源';
  }

  SourceSite toSourceSite() =>
      SourceSite(sourceId, name, 'drpy 脚本 · $detailLabel');

  factory DrpySite.fromJson(Map<String, dynamic> json) => DrpySite(
    key: json['key'] as String? ?? '',
    name: json['name'] as String? ?? '',
    scriptUrl: json['scriptUrl'] as String? ?? '',
    title: json['title'] as String? ?? '',
    host: json['host'] as String? ?? '',
    count: intValue(json['count']),
    engine: json['engine'] as String? ?? '',
  );
}

/// 导入结果：成功入库的脚本源（单条）。
class DrpyImportResult {
  const DrpyImportResult({
    required this.key,
    required this.name,
    this.engine = '',
  });

  final String key;
  final String name;

  /// 脚本方言：空 = drpy2，`catvod` = CatVod 蜘蛛。
  final String engine;

  factory DrpyImportResult.fromJson(Map<String, dynamic> json) =>
      DrpyImportResult(
        key: json['key'] as String? ?? '',
        name: json['name'] as String? ?? '',
        engine: json['engine'] as String? ?? '',
      );
}
