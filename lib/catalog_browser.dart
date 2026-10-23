import 'dart:math';

import 'core_bridge.dart';
import 'models.dart';
import 'source_health.dart';

String categoryName(String name) {
  final compact = name.trim().replaceAll(RegExp(r'\s+'), '');
  return switch (compact) {
    'AI成人短剧' || 'AI短剧' => 'AI 短剧',
    'AI成人漫剧' || 'AI漫剧' => 'AI 漫剧',
    'AI换脸' => 'AI 换脸',
    'AI魔改' => 'AI 魔改',
    'AI剧' => 'AI 剧',
    '' => '未分类',
    _ => name.trim(),
  };
}

class _CatalogChoice {
  _CatalogChoice(this.category);
  final CatalogCategory category;
  final requests = <String, String>{};
}

class _CatalogEntry {
  List<Drama> items = [];
  int page = 0;
  int nextPage = 1;
  bool hasMore = true;
  bool fresh = false;
}

class _CatalogSession {
  final entries = <String, _CatalogEntry>{};

  /// 实际「先拿到内容」的源顺序。
  ///
  /// 展示顺序按它排，而不是按请求顺序：请求顺序由健康度决定（快的先发），
  /// 但如果按请求顺序拼列表，一个排在前面的慢源晚到时会把已经显示出来的
  /// 内容整体往下挤——每来一批源就重排一次，列表看起来一直在跳。按到达
  /// 顺序拼则可以保证「已经显示的条目位置不动，新内容只往后追加」。
  final arrival = <String>[];

  /// 已经展示过的条目 ID 顺序（严格「先到先排」）。
  ///
  /// 只随新条目追加、从不重排：翻页 / 迟到的源都在末尾接上，已有卡片的
  /// 位置不动。条目从源里消失（刷新后换了一批）时顺带清理。
  List<String> order = <String>[];

  int generation = 0;
}

class CatalogBrowser {
  CatalogBrowser(
    this.repository, {
    SourceHealthBook? health,
    this.onHealth,
  }) : health = health ?? SourceHealthBook();

  final AppRepository repository;

  /// 源级健康度账本：排序 + 冷却跳过（详见 source_health.dart）。
  final SourceHealthBook health;

  /// 一次请求结束后回调（source, ok, elapsedMs），由 LocalStore 负责合并落盘。
  final void Function(String source, bool ok, int elapsedMs)? onHealth;
  final _menus = <String, List<CatalogCategory>>{};
  final _library = <String, Map<String, Drama>>{};
  final _sessions = <String, _CatalogSession>{};
  int _generation = 0;

  // 分类聚合的缓存。
  //
  // `categories()` 在首页 build 里被访问（分类标签栏），而 `_choices()` 要扫
  // 一遍「所有源的分类 + 本机剧库的全部条目」。单源时代这很便宜；「全部站源」
  // 有几十个源、上千条剧时，每帧重算就直接吃掉一帧的预算。用版本号兜住：
  // 只有菜单 / 剧库真的变了才重算。
  int _menuRevision = 0;
  int _libraryRevision = 0;
  String _choicesKey = '';
  List<_CatalogChoice>? _choicesCache;

  /// 正在读取的本机缓存（source|category → Future）。
  ///
  /// 启动时首页的目录加载与分类加载是并行的，两边都会按源回读整份缓存；
  /// 合流后同一份数据只跨一次 FFI，几十个源能省掉一半传输。
  final Map<String, Future<CatalogPage>> _cacheFlights = {};

  Future<void> cancel() {
    _generation++;
    for (final session in _sessions.values) {
      session.generation++;
    }
    return repository.cancelCatalog();
  }

  Future<CatalogPage> _readCache(String source, String category) {
    final key = '$source\u0000$category';
    final existing = _cacheFlights[key];
    if (existing != null) return existing;
    final future = repository.cached(source, category: category);
    _cacheFlights[key] = future;
    future.whenComplete(() {
      if (identical(_cacheFlights[key], future)) _cacheFlights.remove(key);
    }).ignore();
    return future;
  }

  void _recordHealth(String source, {required bool ok, required int elapsedMs}) {
    health.record(source, ok: ok, elapsedMs: elapsedMs);
    onHealth?.call(source, ok, elapsedMs);
  }

  /// 记下这个源「第一次在本轮会话里出内容」的位置（只记一次）。
  void _arrive(_CatalogSession session, String source) {
    if (session.entries[source]!.items.isEmpty) return;
    if (!session.arrival.contains(source)) session.arrival.add(source);
  }

  void _remember(String source, Iterable<Drama> items) {
    final library = _library.putIfAbsent(source, () => {});
    var changed = false;
    for (final drama in items) {
      if (drama.source == source) {
        library[drama.id] = library[drama.id]?.merge(drama) ?? drama;
        changed = true;
      }
    }
    if (changed) _libraryRevision++;
  }

  void updateDrama(Drama drama) {
    updateDramas([drama]);
  }

  /// 站源任务把磁盘缓存换新之后调用：丢掉这些源在内存里的旧条目，
  /// 下一次 [load]（cacheOnly / useCache）才会重新回读磁盘。
  ///
  /// 为什么需要它：`load` 的缓存阶段现在会跳过「内存里已有条目」的源
  /// （省掉几十份全量 JSON 的跨 FFI 传输），而站源更新恰恰是**外部**
  /// 改写了磁盘缓存——不显式失效就会一直显示更新前的旧列表。
  void invalidateCached(Iterable<String> sources) {
    final targets = sources.toSet();
    if (targets.isEmpty) return;
    for (final session in _sessions.values) {
      for (final source in targets) {
        final entry = session.entries[source];
        if (entry == null) continue;
        entry.items = [];
        entry.page = 0;
        entry.nextPage = 1;
        entry.hasMore = true;
        entry.fresh = false;
      }
    }
  }

  void updateDramas(Iterable<Drama> dramas) {
    final updates = {for (final drama in dramas) drama.id: drama};
    for (final drama in updates.values) {
      final library = _library[drama.source];
      if (library?.containsKey(drama.id) == true) {
        library![drama.id] = library[drama.id]!.merge(drama);
      }
    }
    for (final session in _sessions.values) {
      for (final entry in session.entries.values) {
        entry.items = [
          for (final item in entry.items)
            updates[item.id] == null ? item : item.merge(updates[item.id]!),
        ];
      }
    }
  }

  /// 并发上限：与 Go 侧请求限流器的全局上限保持一致（app_runtime.go）。
  ///
  /// 「全部站源」分组可能包含几十个运行时片源（libretv / drpy / 规则源），
  /// 每个源一次网络请求。原来这里是 2、Go 侧全局只放行 3，几十个源就是
  /// 排一队等网络——表现出来就是长时间转圈（反馈里的「卡死」）。
  /// 单个站点不会被打爆：Go 侧的限流器按 host 分桶，同一站点仍然串行且
  /// 两次请求之间有最小间隔，这里提高的是「不同站点之间」的并行度。
  static const _concurrency = 8;

  /// 流式渲染的合并间隔。首个源立即回调，之后至少隔这么久才再推一次，
  /// 避免几十个源各触发一次 setState（每次都要重建列表）。
  static const _partialInterval = Duration(milliseconds: 90);

  /// 逐个处理 [entries]，每个完成后回调 [onProgress]（用于流式渲染）。
  ///
  /// [onProgress] 在 visit 结束后**无论成功失败**都会调用：一个源挂了不该
  /// 拖住其他源的结果展示。
  Future<void> _each<T>(
    List<T> entries,
    Future<void> Function(T) visit, {
    void Function()? onProgress,
  }) async {
    if (entries.isEmpty) return;
    var index = 0;
    Future<void> worker() async {
      while (index < entries.length) {
        final entry = entries[index++];
        try {
          await visit(entry);
        } finally {
          onProgress?.call();
        }
      }
    }

    await Future.wait(
      List.generate(min(_concurrency, entries.length), (_) => worker()),
    );
  }

  Future<String?> loadCategories(
    SourceGroup group, {
    bool force = false,
    bool cacheOnly = false,
  }) async {
    final failures = <String>[];
    await _each(group.sources, (source) async {
      // 本机剧库已经读过这个源（目录加载先读的缓存）就不再回读一遍：
      // 启动时 `_load` 与 `_loadCategories` 是并行的，两边都按源读整份缓存，
      // 「全部站源」就是几十份全量 JSON 的重复传输。
      if (_library[source.id]?.isNotEmpty != true) {
        try {
          final cached = await _readCache(source.id, '');
          _remember(source.id, cached.items);
        } catch (_) {}
      }
      // 分类已经加载过且不是强制刷新：省掉网络请求。
      // 每次进软件 / 切分组都向几十个站源拉一遍分类是主要的性能黑洞。
      // 注意只跳网络，上面的本地缓存读取保留——「按分类筛选本机剧库」
      // 依赖 _library，不能一起跳过。
      if (!force && _menus.containsKey(source.id)) return;
      if (cacheOnly) return;
      try {
        _menus[source.id] = await repository.categories(
          source.id,
          force: force,
        );
        _menuRevision++;
      } catch (_) {
        // 请求失败时不要把空结果写进缓存，否则下次进来还是没有分类。
        if (!_menus.containsKey(source.id)) {
          failures.add(source.groupName);
        }
      }
    });
    return failures.isEmpty ? null : '部分分类暂未加载，点击重试';
  }

  List<_CatalogChoice> _choices(SourceGroup group) {
    final key =
        '${group.id}|${group.sources.map((s) => s.id).join(',')}|'
        '$_menuRevision|$_libraryRevision';
    final cached = _choicesCache;
    if (cached != null && key == _choicesKey) return cached;
    final choices = <String, _CatalogChoice>{};
    for (final source in group.sources) {
      for (final category in _menus[source.id] ?? const <CatalogCategory>[]) {
        if (category.id.isEmpty) continue;
        final name = categoryName(category.name);
        final choice = choices.putIfAbsent(
          name,
          () => _CatalogChoice(CatalogCategory('category:$name', name)),
        );
        choice.requests[source.id] = category.id;
      }
    }
    final names = {
      for (final source in group.sources)
        for (final item in _library[source.id]?.values ?? const <Drama>[])
          categoryName(item.category),
    }.toList()..sort();
    for (final name in names) {
      choices.putIfAbsent(
        name,
        () => _CatalogChoice(CatalogCategory('local:$name', name, local: true)),
      );
    }
    final result = choices.values.toList();
    _choicesCache = result;
    _choicesKey = key;
    return result;
  }

  List<CatalogCategory> categories(SourceGroup group) => [
    CatalogCategory.all,
    for (final choice in _choices(group)) choice.category,
  ];

  _CatalogChoice? _choice(SourceGroup group, String category) => _choices(
    group,
  ).where((choice) => choice.category.id == category).firstOrNull;

  Future<CatalogPage> load(
    SourceGroup group, {
    String category = '',
    String query = '',
    bool more = false,
    bool force = false,
    bool useCache = false,
    bool cacheOnly = false,
    void Function(CatalogPage)? onCached,
    // 流式渲染：**每有一个源返回就回调一次**，让「全部站源」这种几十个源的
    // 场景先出内容、再逐步补齐，而不是等所有源都回来才显示。
    // 内部按 [_partialInterval] 节流，但第一个源永远立即回调。
    void Function(CatalogPage)? onPartial,
  }) async {
    if (cacheOnly && (more || force || query.trim().isNotEmpty)) {
      throw AppFailure('缓存读取不能同时请求搜索或续页');
    }
    final request = ++_generation;
    for (final session in _sessions.values) {
      session.generation++;
    }
    await repository.cancelCatalog();
    if (request != _generation) throw AppFailure('已取消加载');
    final choice = query.isEmpty ? _choice(group, category) : null;
    final requests = choice != null && !choice.category.local
        ? choice.requests
        : {for (final source in group.sources) source.id: ''};
    final key =
        '${group.sources.map((s) => s.id).join(',')}|'
        '${choice?.category.local == false ? category : ''}|$query';
    final session = _sessions.putIfAbsent(key, _CatalogSession.new);
    final generation = ++session.generation;
    final failures = <String, String>{};
    final requested = requests.keys.toList();
    // 按健康度排序：健康的先发、死源排最后（并发槽位先给能出结果的源）。
    // 只有「被动加载」（进首页 / 切分类 / 搜索）才允许跳过冷却中的死源：
    //   - force（下拉刷新、手动重试）→ 全部重试；
    //   - more（续页）→ 保持原顺序，别把正在翻的源换掉；
    //   - 单源分组 → 用户就是冲着这个源来的，永远不跳过。
    final healthPlan = health.plan(
      requested,
      force: force || more || cacheOnly,
      allowSkip: !more && !cacheOnly && requested.length > 1,
    );
    final sourceOrder = healthPlan.order;
    // 被跳过的源仍保留上次的条目继续展示（只是不再等它），排到队尾。
    final displayOrder = [...sourceOrder, ...healthPlan.skipped];
    if (healthPlan.skipped.isNotEmpty) {
      failures['#health'] =
          '${healthPlan.skipped.length} 个源连续失败，已暂时跳过（下拉刷新可重试）';
    }
    for (final source in displayOrder) {
      session.entries.putIfAbsent(source, _CatalogEntry.new);
    }

    CatalogPage snapshot() {
      // 到达顺序优先；从没出过内容的源（含被跳过的旧条目）排在后面。
      // 这里不做「按行轮流取一条」的交错：交错在流式阶段会让每批新内容
      // 插进已有条目中间，整屏往下窜。追加式排列对「只在加载中看」最友好。
      final order = <String>[
        ...session.arrival,
        for (final source in displayOrder)
          if (!session.arrival.contains(source)) source,
      ];
      final current = <String, Drama>{};
      for (final source in order) {
        for (final item in session.entries[source]!.items) {
          current[item.id] = item;
        }
      }
      // 沿用上次展示顺序（只保留还在的条目），新条目按来源顺序追加到末尾。
      final items = <Drama>[];
      final seen = <String>{};
      for (final id in session.order) {
        final item = current[id];
        if (item != null && seen.add(id)) items.add(item);
      }
      for (final source in order) {
        for (final item in session.entries[source]!.items) {
          if (seen.add(item.id)) items.add(item);
        }
      }
      if (choice != null) {
        for (final source in group.sources) {
          for (final item in _library[source.id]?.values ?? const <Drama>[]) {
            if ((choice.category.local ||
                    categoryName(item.category) == choice.category.name) &&
                seen.add(item.id)) {
              items.add(item);
            }
          }
        }
      }
      session.order = [for (final item in items) item.id];
      return CatalogPage(
        items,
        // 新鲜度 / 续页只看真正请求过的源：被跳过的源不应让「还有更多」永远为真。
        hasMore: sourceOrder.any((source) => session.entries[source]!.hasMore),
        page: sourceOrder.fold<int>(
          1,
          (page, source) => max(page, session.entries[source]!.page),
        ),
        fresh: sourceOrder.every((source) => session.entries[source]!.fresh),
        warning: failures.values.toSet().join('；'),
      );
    }

    // 流式发射器：首个源完成立即发，之后按间隔合并，避免几十次 setState。
    var lastEmit = 0;
    var emitted = false;
    void emitPartial() {
      final callback = onPartial;
      if (callback == null) return;
      final now = DateTime.now().millisecondsSinceEpoch;
      if (emitted && now - lastEmit < _partialInterval.inMilliseconds) return;
      emitted = true;
      lastEmit = now;
      if (generation != session.generation) return;
      callback(snapshot());
    }

    if ((useCache || cacheOnly) && !force && !more && query.isEmpty) {
      // 已经在本轮会话里有条目的源不再回读磁盘缓存：`cached` 是按源返回
      // **整份**列表的一次跨 FFI/JSON 调用，几十个源就是几十份全量数据。
      // 内存里有就直接用内存，没有才读。
      final pending = [
        for (final source in sourceOrder)
          if (session.entries[source]!.items.isEmpty) source,
      ];
      await _each(pending, onProgress: emitPartial, (source) async {
        try {
          final cached = await _readCache(source, requests[source]!);
          if (generation != session.generation || cached.items.isEmpty) return;
          final entry = session.entries[source]!;
          entry.items = cached.items;
          _arrive(session, source);
          entry.page = cached.page;
          entry.nextPage = cached.page + 1;
          entry.hasMore = cached.hasMore || cached.warning.isNotEmpty;
          entry.fresh = cached.fresh && cached.warning.isEmpty;
          if (cached.warning.isNotEmpty) failures[source] = cached.warning;
          _remember(source, cached.items);
        } catch (error) {
          if (cacheOnly) failures[source] = error.toString();
        }
      });
      if (generation != session.generation) return snapshot();
      final cached = snapshot();
      if (cached.items.isNotEmpty) onCached?.call(cached);
      if (cacheOnly) return cached;
      if (cached.fresh && cached.items.isNotEmpty) return cached;
    }

    await _each(sourceOrder, onProgress: emitPartial, (source) async {
      if (generation != session.generation) return;
      final entry = session.entries[source]!;
      if (more && !entry.hasMore) return;
      if (useCache && !force && !more && entry.fresh) return;
      final page = more ? entry.nextPage : 1;
      final started = DateTime.now();
      try {
        final result = await repository.catalog(
          source,
          category: requests[source]!,
          query: query,
          page: page,
          force: force,
        );
        _recordHealth(
          source,
          ok: result.warning.isEmpty,
          elapsedMs: DateTime.now().difference(started).inMilliseconds,
        );
        if (generation != session.generation) return;
        final items = <String, Drama>{
          if (more)
            for (final item in entry.items) item.id: item,
          for (final item in result.items) item.id: item,
        };
        entry.items = items.values.toList();
        _arrive(session, source);
        entry.page = result.page;
        entry.nextPage = result.warning.isEmpty ? result.page + 1 : page;
        entry.hasMore =
            (query.isEmpty || SourceSite.byId(source).pagedSearch) &&
            (result.hasMore || result.warning.isNotEmpty);
        entry.fresh = result.fresh && result.warning.isEmpty;
        if (query.isEmpty) _remember(source, result.items);
        if (result.warning.isNotEmpty) {
          failures[source] = result.warning;
        } else {
          failures.remove(source);
        }
      } catch (error) {
        _recordHealth(
          source,
          ok: false,
          elapsedMs: DateTime.now().difference(started).inMilliseconds,
        );
        if (generation != session.generation) return;
        entry.nextPage = page;
        entry.hasMore = query.isEmpty || SourceSite.byId(source).pagedSearch;
        entry.fresh = false;
        failures[source] = error.toString();
      }
    });
    final result = snapshot();
    if (result.items.isEmpty && failures.isNotEmpty) {
      throw AppFailure(result.warning);
    }
    return result;
  }
}
