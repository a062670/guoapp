import 'dart:convert';

import 'app_build.dart';

class SourceSite {
  const SourceSite(this.id, this.name, this.description);
  final String id;
  final String name;
  final String description;
  bool get onlineSearch => id == 'hongguo' || pagedSearch;
  bool get pagedSearch =>
      id == 'huangju' ||
      id == 'yeguo' ||
      id == 'dsd' ||
      id == 'sorani' ||
      id == 'guipian' ||
      id == 'hanxiaoquan' ||
      isRuntime;
  bool get searchSuggestions => id == 'hongguo';

  /// 运行时导入的 LibreTV 片源（`libretv:<key>`）与 drpy 脚本源
  /// （`drpy:<key>`），由导入注册。
  bool get isRuntime => id.startsWith('libretv:') || id.startsWith('drpy:');
  String get groupId => switch (id) {
    'huangguo-video' || 'huangguoai' || 'cloudfront' => 'huangguo',
    _ => isRuntime ? (id.startsWith('drpy:') ? 'drpy' : 'libretv') : id,
  };
  String get groupName => switch (groupId) {
    'huangguo' => '黄果',
    'libretv' => 'LibreTV 片源',
    'drpy' => 'drpy 脚本源',
    _ => name,
  };
  String get entryName => switch (id) {
    'huangguo-video' => '视频',
    'huangguoai' => 'AI',
    'cloudfront' => '旧版',
    _ => name,
  };

  static const hongguo = SourceSite('hongguo', '红果', '短剧 · 漫剧 · AI 剧');
  static const dsd = SourceSite('dsd', '帝果', '分类视频 · 在线搜索');
  static const sorani = SourceSite('sorani', '青空', '番剧 · 剧场动画 · 特摄');
  static const guipian = SourceSite('guipian', '鬼片', '鬼片 · 电视剧 · 动漫');
  static const hanxiaoquan = SourceSite(
    'hanxiaoquan',
    '韩小圈',
    '韩剧 · 韩国电影 · 综艺动漫',
  );

  /// 默认可见的站源：红果、韩小圈、鬼片网、青空次元。
  static const primaryValues = [hongguo, hanxiaoquan, guipian, sorani];

  /// 敏感站源：默认隐藏，输入解锁密码后才显示。
  static const restrictedValues = [
    SourceSite('huangdou', '黄豆', '精选短剧'),
    SourceSite('huangju', '剧果', '热门 · 最新 · 分类短剧'),
    SourceSite('yeguo', '野果', '分类短剧 · 在线搜索'),
    dsd,
    SourceSite('huangguo-video', '黄果视频', '视频剧集'),
    SourceSite('huangguoai', '黄果 AI', 'AI 短剧'),
    SourceSite('cloudfront', '黄果旧版', '旧 API 剧库'),
  ];
  static const knownValues = [...primaryValues, ...restrictedValues];
  static const allValues = [...primaryValues, ...restrictedValues];

  /// 运行时注册的 LibreTV 站源（用户导入；进程内有效，重启后由原生核心
  /// libretv.json 重新拉取注册）。键为完整源 ID「libretv:」+ 站点 key。
  static final _runtimeSources = <String, SourceSite>{};

  /// 用一份站点列表整体替换运行时注册表（应用启动时同步原生核心、
  /// 导入 / 删除后刷新界面用）。
  static void setRuntimeSources(Iterable<SourceSite> sites) {
    _runtimeSources.clear();
    for (final site in sites) {
      if (site.isRuntime) _runtimeSources[site.id] = site;
    }
    _invalidateSourceCaches();
  }

  /// 注销一个运行时站源（删除 LibreTV 片源用）。
  static void unregisterRuntime(String id) {
    _runtimeSources.remove(id.trim());
    _invalidateSourceCaches();
  }

  /// 当前已注册的运行时站源列表（按 ID 排序，输出稳定）。
  static List<SourceSite> get runtimeValues {
    final keys = _runtimeSources.keys.toList()..sort();
    return [for (final key in keys) _runtimeSources[key]!];
  }

  /// 运行时 + 内置的全部可见站源。
  static List<SourceSite> get values => [
    ...(allSourcesEnabled ? knownValues : [hongguo]),
    ...runtimeValues,
  ];
  /// 站源 ID 集合缓存。
  ///
  /// `isAvailable` / `isPrimary` 会被 `allowsSource` **逐条**调用：首页
  /// 「全部站源」一次刷新上千条，每条都重建一遍 `values`（含运行时源排序）
  /// 再线性查找，等于每帧做几千次「建列表 + 排序」——这是加载期间卡顿的
  /// 一个真实来源。注册表只在导入 / 删除运行时源时变化，用版本缓存掉。
  static Set<String>? _availableCache;
  static Set<String>? _primaryCache;

  static void _invalidateSourceCaches() {
    _availableCache = null;
    _primaryCache = null;
  }

  static bool isAvailable(String id) =>
      (_availableCache ??= {for (final site in values) site.id}).contains(id);
  static bool isKnown(String id) =>
      allValues.any((site) => site.id == id) || _runtimeSources.containsKey(id);
  static bool isPrimary(String id) =>
      (_primaryCache ??= {for (final site in primaryValues) site.id}).contains(id);
  static bool isRestricted(String id) =>
      restrictedValues.any((site) => site.id == id);

  /// 运行时可见站源：未解锁时保留 [primaryValues] 与用户导入的 LibreTV 片源
  /// （导入行为即用户授权，不占用敏感站源解锁状态）。
  static List<SourceSite> visibleValues({required bool unlocked}) => [
    for (final site in values)
      if (unlocked || isPrimary(site.id) || site.isRuntime) site,
  ];
  static SourceSite byId(String id) {
    final runtime = _runtimeSources[id];
    if (runtime != null) return runtime;
    return allValues.firstWhere((site) => site.id == id, orElse: () => hongguo);
  }
}

class SourceGroup {
  const SourceGroup(this.id, this.name, this.sources);
  final String id;
  final String name;
  final List<SourceSite> sources;

  static List<SourceGroup> fromSources(
    Iterable<SourceSite> sources, {
    Set<String> favoriteIds = const {},
  }) {
    final groups = <String, List<SourceSite>>{};
    for (final source in sources) {
      // 收藏的运行时片源（libretv:/drpy:）不并入 LibreTV / drpy 大组，
      // 独占一组、组名即源名，与红果等内置源同级出现在片源选择里。
      final favorite = favoriteIds.contains(source.id);
      final groupId = favorite ? source.id : source.groupId;
      (groups[groupId] ??= []).add(source);
    }
    return [
      for (final group in groups.entries)
        SourceGroup(
          group.key,
          favoriteIds.contains(group.value.first.id)
              ? group.value.first.name
              : group.value.first.groupName,
          group.value,
        ),
    ];
  }
}

class CatalogCategory {
  const CatalogCategory(this.id, this.name, {this.local = false});
  static const all = CatalogCategory('', '全部');
  final String id;
  final String name;
  final bool local;
  factory CatalogCategory.fromJson(Map<String, dynamic> json) =>
      CatalogCategory(
        json['id'] as String? ?? '',
        json['name'] as String? ?? '全部',
      );
}

int intValue(Object? value) =>
    value is num ? value.toInt() : int.tryParse('$value') ?? 0;

class Drama {
  const Drama({
    required this.id,
    required this.source,
    required this.title,
    this.sourceId = '',
    this.description = '',
    this.cover = '',
    this.episodes = 0,
    this.category = '',
    bool? vip,
    this.score = '',
    this.actor = '',
    this.director = '',
    this.heat = '',
    this.views = '',
    this.onlineDate = '',
    this.tags = const [],
    this.releaseStatus = '',
    this.updateTime = '',
  }) : vipStatus = vip;
  final String id;
  final String source;
  final String sourceId;
  final String title;
  final String description;
  final String cover;
  final int episodes;
  final String category;
  final bool? vipStatus;
  bool get vip => vipStatus == true;
  final String score;
  final String actor;
  final String director;
  final String heat;
  final String views;
  final String onlineDate;
  final List<String> tags;
  final String releaseStatus;

  /// 源站更新时间（MacCMS 的 vod_time / XML 的 last），原始字符串，
  /// 形如「2026-10-19 20:15」。仅展示用，不参与排序计算。
  final String updateTime;
  String get releaseLabel => switch (releaseStatus) {
    'finished' || 'completed' => '已完结',
    'ongoing' => '连载中',
    _ => '状态未知',
  };

  factory Drama.fromJson(Map<String, dynamic> json) => Drama(
    id: json['id'] as String? ?? '',
    source: json['source'] as String? ?? 'hongguo',
    sourceId: json['sourceId'] as String? ?? '',
    title: json['title'] as String? ?? '短剧',
    description: json['description'] as String? ?? '',
    cover: json['cover'] as String? ?? '',
    episodes: intValue(json['episodes']),
    category: json['category'] as String? ?? '',
    vip: json['vip'] == true
        ? true
        : json['vip'] == false &&
              (json['source'] != 'huangdou' ||
                  intValue(json['metadataSchema']) >= 1)
        ? false
        : null,
    heat: json['heat']?.toString() ?? '',
    // score 在个别源里是数字，统一转字符串；旧备份没有这些键，缺省空串。
    score: json['score']?.toString() ?? '',
    actor: json['actor']?.toString() ?? '',
    director: json['director']?.toString() ?? '',
    views: json['views']?.toString() ?? '',
    onlineDate: json['onlineDate'] as String? ?? '',
    tags: (json['tags'] as List? ?? const []).whereType<String>().toList(),
    releaseStatus: json['releaseStatus'] as String? ?? '',
    updateTime: json['updateTime'] as String? ?? '',
  );
  Map<String, dynamic> toJson() => {
    'metadataSchema': 1,
    'id': id,
    'source': source,
    'sourceId': sourceId,
    'title': title,
    'description': description,
    'cover': cover,
    'episodes': episodes,
    'category': category,
    'vip': vipStatus,
    'heat': heat,
    'score': score,
    'actor': actor,
    'director': director,
    'views': views,
    'onlineDate': onlineDate,
    'tags': tags,
    'releaseStatus': releaseStatus,
    'updateTime': updateTime,
  };

  Drama merge(Drama fresh) {
    if (id != fresh.id) return fresh;
    const genericCategories = {
      '短剧',
      '真人剧',
      '漫剧',
      'AI剧',
      'AI 剧',
      'AI短剧',
      'AI 短剧',
      'AI漫剧',
      'AI 漫剧',
    };
    return Drama(
      id: id,
      source: fresh.source.isEmpty ? source : fresh.source,
      sourceId: fresh.sourceId.isEmpty ? sourceId : fresh.sourceId,
      title: fresh.title.isEmpty || fresh.title == '短剧' ? title : fresh.title,
      description: fresh.description.isEmpty ? description : fresh.description,
      cover: fresh.cover.isEmpty ? cover : fresh.cover,
      episodes: fresh.episodes > 0 ? fresh.episodes : episodes,
      category:
          fresh.category.isEmpty ||
              genericCategories.contains(fresh.category) &&
                  category.isNotEmpty &&
                  !genericCategories.contains(category)
          ? category
          : fresh.category,
      vip: fresh.vipStatus ?? vipStatus,
      heat: fresh.heat.isEmpty ? heat : fresh.heat,
      score: fresh.score.isEmpty ? score : fresh.score,
      actor: fresh.actor.isEmpty ? actor : fresh.actor,
      director: fresh.director.isEmpty ? director : fresh.director,
      views: fresh.views.isEmpty ? views : fresh.views,
      onlineDate: fresh.onlineDate.isEmpty ? onlineDate : fresh.onlineDate,
      tags: fresh.tags.isEmpty ? tags : fresh.tags,
      releaseStatus:
          fresh.releaseStatus.isEmpty || fresh.releaseStatus == 'unknown'
          ? releaseStatus
          : fresh.releaseStatus,
      updateTime: fresh.updateTime.isEmpty ? updateTime : fresh.updateTime,
    );
  }
}

class Episode {
  Episode(this.raw, int fallback)
    : id = raw['id'] as String? ?? '',
      title = raw['title'] as String? ?? '第$fallback集',
      number = intValue(raw['currentEpisode']) > 0
          ? intValue(raw['currentEpisode'])
          : fallback,
      vip = raw['vip'] == true;
  final Map<String, dynamic> raw;
  final String id;
  final String title;
  final int number;
  final bool vip;
}

class DramaDetail {
  DramaDetail(this.drama, this.episodes, {this.warning = ''});
  final Drama drama;
  final List<Episode> episodes;
  final String warning;
  factory DramaDetail.fromJson(Map<String, dynamic> json) {
    final rows = json['chapters'] as List? ?? const [];
    return DramaDetail(
      Drama.fromJson(Map<String, dynamic>.from(json['drama'] as Map)),
      [
        for (var i = 0; i < rows.length; i++)
          Episode(Map<String, dynamic>.from(rows[i] as Map), i + 1),
      ],
      warning: json['warning'] as String? ?? '',
    );
  }
}

class CatalogPage {
  CatalogPage(
    this.items, {
    this.hasMore = false,
    this.warning = '',
    this.page = 1,
    this.fresh = false,
  });
  final List<Drama> items;
  final bool hasMore;
  final String warning;
  final int page;
  final bool fresh;
  factory CatalogPage.fromJson(Map<String, dynamic> json) => CatalogPage(
    [
      for (final row in json['items'] as List? ?? const [])
        Drama.fromJson(Map<String, dynamic>.from(row as Map)),
    ],
    hasMore: json['hasMore'] == true,
    warning: json['warning'] as String? ?? '',
    page: intValue(json['page']) > 0 ? intValue(json['page']) : 1,
    fresh: json['fresh'] == true,
  );
}

class PlaybackPlan {
  const PlaybackPlan({
    required this.url,
    this.headers = const {},
    this.decryptionKey = '',
    this.quality = 0,
    this.qualities = const [],
    this.session = '',
    this.danmakuId = '',
    this.prefetchedBytes = 0,
    this.expiresAt = 0,
    this.routeIndex = 0,
    this.routeCount = 1,
    this.verified = false,
    this.local = false,
  });
  final String url;
  final Map<String, String> headers;
  final String decryptionKey;
  final int quality;
  final List<int> qualities;
  final String session;
  final String danmakuId;
  final int prefetchedBytes;
  final int expiresAt;
  final int routeIndex;
  final int routeCount;

  /// 预加载已确认这条线路能取到数据（首字节在预算内）。
  /// 为真时播放器把首帧等待放宽，减少对"慢但可用"线路的误杀。
  final bool verified;
  final bool local;

  /// 还能不能切到别的线路。线路表按环形处理：最后一条也能回到首选线路，
  /// 否则记忆停在备用线时失败只能反复重解析同一条线。
  bool get hasAlternative => session.isNotEmpty && routeCount > 1;
  factory PlaybackPlan.fromJson(Map<String, dynamic> json) => PlaybackPlan(
    url: json['url'] as String? ?? '',
    local: json['local'] == true,
    headers: (json['headers'] as Map? ?? {}).map(
      (key, value) => MapEntry(key.toString(), value.toString()),
    ),
    decryptionKey: json['decryptionKey'] as String? ?? '',
    quality: intValue(json['quality']),
    qualities: (json['qualities'] as List? ?? []).map(intValue).toSet().toList()
      ..sort((a, b) => b.compareTo(a)),
    session: json['session'] as String? ?? '',
    danmakuId: json['danmakuId'] as String? ?? '',
    prefetchedBytes: intValue(json['prefetchedBytes']),
    expiresAt: intValue(json['expiresAt']),
    routeIndex: intValue(json['routeIndex']),
    routeCount: intValue(json['routeCount']) > 0
        ? intValue(json['routeCount'])
        : 1,
    verified: json['verified'] == true,
  );
}

class WatchEntry {
  WatchEntry({
    required this.drama,
    required this.episode,
    required this.position,
    required this.duration,
    required this.updatedAt,
  });
  final Drama drama;
  final int episode;
  final double position;
  final double duration;
  final DateTime updatedAt;
  bool get finished => duration > 1 && position >= duration - 1;
  Map<String, dynamic> toJson() => {
    'drama': drama.toJson(),
    'episode': episode,
    'position': position,
    'duration': duration,
    'updatedAt': updatedAt.toIso8601String(),
  };
  factory WatchEntry.fromJson(Map<String, dynamic> json) => WatchEntry(
    drama: Drama.fromJson(Map<String, dynamic>.from(json['drama'] as Map)),
    episode: intValue(json['episode']),
    position: (json['position'] as num?)?.toDouble() ?? 0,
    duration: (json['duration'] as num?)?.toDouble() ?? 0,
    updatedAt:
        DateTime.tryParse(json['updatedAt'].toString()) ?? DateTime(2000),
  );
}

class DownloadJob {
  const DownloadJob({
    required this.id,
    required this.drama,
    required this.episode,
    required this.state,
    this.bytes = 0,
    this.total = 0,
    this.progress = 0,
    this.quality = 0,
    this.actualQuality = 0,
    this.error = '',
    this.created = 0,
    this.revision = 0,
    this.archived = false,
  });
  final int created;
  final int revision;
  final bool archived;
  final String id;
  final Drama drama;
  final Episode episode;
  final String state;
  final int bytes;
  final int total;
  final double progress;
  final int quality;
  final int actualQuality;
  final String error;
  bool get completed => state == 'completed';
  bool get active => state == 'queued' || state == 'downloading';
  bool get resumable => state == 'paused' || state == 'failed';
  String get stateLabel => switch (state) {
    'queued' => '等待下载',
    'downloading' => '正在下载',
    'paused' => '已暂停',
    'failed' => '下载失败',
    'completed' => '已下载',
    'removing' => '正在取消',
    _ => '等待更新',
  };
  factory DownloadJob.fromJson(Map<String, dynamic> value) => DownloadJob(
    id: value['id'] as String? ?? '',
    drama: Drama.fromJson(Map<String, dynamic>.from(value['drama'] as Map)),
    episode: Episode(
      Map<String, dynamic>.from(value['chapter'] as Map),
      intValue(value['index']),
    ),
    state: value['state'] as String? ?? 'failed',
    bytes: intValue(value['bytes']),
    total: intValue(value['total']),
    progress: ((value['progress'] as num?)?.toDouble() ?? 0).clamp(0, 1),
    quality: intValue(value['quality']),
    actualQuality: intValue(value['actualQuality']),
    error: value['error'] as String? ?? '',
    created: intValue(value['created']),
    revision: intValue(value['revision']),
    archived: value['archived'] == true,
  );
}

List<Map<String, dynamic>> readJsonList(String? value) {
  try {
    return (jsonDecode(value ?? '[]') as List)
        .whereType<Map>()
        .map((row) => Map<String, dynamic>.from(row))
        .toList();
  } catch (_) {
    return [];
  }
}
