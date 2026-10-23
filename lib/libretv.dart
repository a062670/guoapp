import 'models.dart';

/// 一个已导入的 LibreTV（Apple CMS / MacCMS provide/vod）片源。
/// 对应原生核心 libretv.json 里的一条站点记录。
class LibreTVSite {
  const LibreTVSite({
    required this.key,
    required this.name,
    required this.api,
    this.type = 1,
    this.count = 0,
    this.engine = '',
    this.ruleUrl = '',
  });

  final String key;
  final String name;

  /// 采集接口地址（provide/vod）。
  final String api;

  /// TVBox 站点类型：1 = JSON 采集接口，2 = XML 采集接口（苹果 CMS at/xml）。
  final int type;

  /// 本机已缓存的剧集数（站源管理里展示）。
  final int count;

  /// 规则引擎：非空表示这是「规则型 Spider」片源（XBPQ / 海阔 / Bili）。
  /// 这类片源的数据由本地规则解释器抓取（规则快照存在 rules/ 下），
  /// 不需要 jar 包与 Java 运行环境；空串 = 普通 MacCMS 采集站。
  final String engine;

  /// 是否规则型 Spider 片源。
  bool get isRule => engine.isNotEmpty;

  /// 规则来源地址（远程规则才有；内联规则为空，无法跨设备重建）。
  final String ruleUrl;

  /// 规则源对应的 TVBox Spider 名（跨设备重建订阅条目时用）。
  String get spiderApi {
    switch (engine) {
      case 'xbpq':
        return 'csp_XBPQ';
      case 'hiker':
        return 'csp_XYQHiker';
      case 'bili':
        return 'csp_Bili';
      default:
        return '';
    }
  }

  /// 跨设备同步用的订阅配置：用规则地址重建与原条目等价的
  /// `{"sites":[{...,"api":"csp_XBPQ","ext":"<规则地址>"}]}`。
  /// 内联规则（没有 ruleUrl）返回空串——别的设备无法重建，
  /// 同步侧会跳过并提示重新导入订阅。
  String get ruleSubscriptionJson {
    if (!isRule || ruleUrl.isEmpty || spiderApi.isEmpty) return '';
    return '{"sites":[{"key":"$key","name":${_jsonString(name)},"type":3,'
        '"api":"$spiderApi","ext":${_jsonString(ruleUrl)}}]}';
  }

  /// 最小 JSON 字符串转义（名称里有引号 / 反斜杠时不破坏配置）。
  static String _jsonString(String value) {
    final escaped = value
        .replaceAll(r'\', r'\\')
        .replaceAll('"', r'\"')
        .replaceAll('\n', r'\n');
    return '"$escaped"';
  }

  /// 完整源 ID，与 [SourceSite] 运行时注册表一致。
  String get sourceId => 'libretv:$key';

  bool get isXml => type == 2;

  /// 类型标签：规则源显示引擎名（XBPQ 规则 / 海阔规则 / Bili），
  /// 采集站沿用 JSON/XML 采集。
  String get typeLabel {
    switch (engine) {
      case 'xbpq':
        return 'XBPQ 规则';
      case 'hiker':
        return '海阔规则';
      case 'bili':
        return 'Bili';
      default:
        return isXml ? 'XML 采集' : 'JSON 采集';
    }
  }

  /// 规则源的引导文案（站源管理 tooltip 用）。
  String get engineHint {
    switch (engine) {
      case 'xbpq':
        return 'XBPQ 规则源：由本地规则解释器抓取（分类/详情/搜索），无需 jar 与 Java';
      case 'hiker':
        return '海阔视界规则源：由本地规则解释器抓取（支持 JSON / HTML 两种截取模式）';
      case 'bili':
        return 'B站规则源：分类直出，搜索/详情走 B 站公开接口，播放由 WebView 嗅探';
      default:
        return '';
    }
  }

  SourceSite toSourceSite() =>
      SourceSite(sourceId, name, 'LibreTV $typeLabel · ${_apiHost(api)}');

  factory LibreTVSite.fromJson(Map<String, dynamic> json) => LibreTVSite(
    key: json['key'] as String? ?? '',
    name: json['name'] as String? ?? '',
    api: json['api'] as String? ?? '',
    type: intValue(json['type']) == 2 ? 2 : 1,
    count: intValue(json['count']),
    engine: json['engine'] as String? ?? '',
    ruleUrl: json['ruleUrl'] as String? ?? '',
  );

  static String _apiHost(String api) {
    final trimmed = api.trim();
    final start = trimmed.indexOf('://');
    if (start < 0) return trimmed;
    final rest = trimmed.substring(start + 3);
    final slash = rest.indexOf('/');
    return slash < 0 ? rest : rest.substring(0, slash);
  }
}

/// 订阅里的一条解析规则（parses）。
///
/// [type] 沿用 TVBox 约定：0 = 嗅探、1 = JSON 解析接口；
/// 本项目扩展：type=2 且 [url] 以 .js 结尾 = 本地解析脚本（非标准语义）。
class LibreTVParse {
  const LibreTVParse({
    required this.name,
    required this.type,
    required this.url,
    this.kind = '',
    this.script = false,
    this.cached = false,
  });

  final String name;
  final int type;
  final String url;

  /// 类型名：本地脚本 / JSON 接口 / 嗅探（Go 侧计算，UI 直接展示）。
  final String kind;

  /// 是否本地脚本扩展类型。
  final bool script;

  /// 本地脚本是否已缓存到设备（远程脚本首次使用时下载）。
  final bool cached;

  factory LibreTVParse.fromJson(Map<String, dynamic> json) => LibreTVParse(
    name: json['name'] as String? ?? '',
    type: intValue(json['type']),
    url: json['url'] as String? ?? '',
    kind: json['kind'] as String? ?? '',
    script: json['script'] == true,
    cached: json['cached'] == true,
  );
}

/// 解析方式配置：已导入的 parses 与当前选择（空 = 直链/嗅探）。
class LibreTVParseConfig {
  const LibreTVParseConfig({this.parses = const [], this.selected = ''});

  final List<LibreTVParse> parses;

  /// 当前选中的解析 URL；空串表示「直链/嗅探」。
  final String selected;

  factory LibreTVParseConfig.fromJson(Map<String, dynamic> json) =>
      LibreTVParseConfig(
        parses: (json['parses'] as List? ?? const [])
            .whereType<Map>()
            .map((row) => LibreTVParse.fromJson(Map<String, dynamic>.from(row)))
            .toList(),
        selected: json['selected'] as String? ?? '',
      );
}

/// 混合订阅里发现的 drpy 脚本候选（type≥3 且 api 是 .js）。
/// 候选不自动导入——脚本需要用户确认后才加入，这里仅携带名称与地址。
class DrpyCandidate {
  const DrpyCandidate({
    required this.name,
    required this.api,
    this.relative = false,
  });

  final String name;
  final String api;

  /// 脚本地址原本是相对路径（`./js/xx.js`），[api] 是按订阅地址补全出来的。
  ///
  /// 补全只在「订阅就是脚本所在站点」时成立。多源聚合订阅（如影视仓聚合
  /// 518 站）把各家配置合并成一个文件却没改写相对路径，补全出来的是聚合
  /// 文件目录下的地址，必然 404——这类站点在原版 TVBox 里同样导入不了。
  /// 标记出来是为了在候选列表里提前提示，而不是等用户点了才报错。
  final bool relative;

  factory DrpyCandidate.fromJson(Map<String, dynamic> json) => DrpyCandidate(
    name: json['name'] as String? ?? '',
    api: json['api'] as String? ?? '',
    relative: json['relative'] as bool? ?? false,
  );
}

/// 导入结果：成功入库的站点、被跳过的条目、drpy 候选、解析规则与探测统计。
class LibreTVImportResult {
  const LibreTVImportResult({
    required this.added,
    required this.skipped,
    required this.drpy,
    this.parses = const [],
    required this.verified,
  });

  final List<LibreTVSite> added;
  final List<String> skipped;

  /// type≥3 的 .js 脚本候选，等待用户确认导入。
  final List<DrpyCandidate> drpy;

  /// 本次订阅里导入的解析规则（同一 URL 会覆盖旧条目）。
  final List<LibreTVParse> parses;
  final int verified;

  bool get isEmpty => added.isEmpty && drpy.isEmpty;

  factory LibreTVImportResult.fromJson(Map<String, dynamic> json) =>
      LibreTVImportResult(
        added: (json['added'] as List? ?? const [])
            .whereType<Map>()
            .map((row) => LibreTVSite.fromJson(Map<String, dynamic>.from(row)))
            .toList(),
        skipped: (json['skipped'] as List? ?? const [])
            .whereType<String>()
            .toList(),
        drpy: (json['drpy'] as List? ?? const [])
            .whereType<Map>()
            .map(
              (row) => DrpyCandidate.fromJson(Map<String, dynamic>.from(row)),
            )
            .toList(),
        parses: (json['parses'] as List? ?? const [])
            .whereType<Map>()
            .map((row) => LibreTVParse.fromJson(Map<String, dynamic>.from(row)))
            .toList(),
        verified: intValue(json['verified']),
      );
}
