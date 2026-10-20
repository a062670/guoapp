import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

import 'core_bridge.dart';
import 'drpy.dart';
import 'libretv.dart';
import 'local_store.dart';
import 'models.dart';
import 'source_status.dart';

String sourceTimestamp(DateTime? value) {
  if (value == null) return '尚无记录';
  final local = value.toLocal();
  String two(int number) => number.toString().padLeft(2, '0');
  return '${local.month}/${local.day} ${two(local.hour)}:${two(local.minute)}';
}

class SourcesScreen extends StatefulWidget {
  const SourcesScreen({
    super.key,
    required this.repository,
    required this.store,
    this.initialSource,
    this.drama,
  });

  final AppRepository repository;
  final LocalStore store;
  final String? initialSource;
  final Drama? drama;

  @override
  State<SourcesScreen> createState() => _SourcesScreenState();
}

class _SourcesScreenState extends State<SourcesScreen> {
  final _statuses = <String, SourceStatus>{};
  final _errors = <String, String>{};
  final _pending = <String>{};
  final _revisions = <String, int>{};
  final _expandedHealth = <String>{};
  Timer? _timer;
  bool _polling = false;
  int _ticks = 0;

  /// 已导入的 LibreTV 站点（来自原生核心 libretv.json，含目录缓存计数）。
  List<LibreTVSite> _libretvSites = const [];

  /// 已导入的 drpy 脚本源（来自原生核心 drpy.json + scripts/）。
  List<DrpySite> _drpySites = const [];

  /// LibreTV 解析方式（订阅 parses + 当前选择）。
  LibreTVParseConfig _parseConfig = const LibreTVParseConfig();
  bool _libretvBusy = false;

  @override
  void initState() {
    super.initState();
    unawaited(_refresh());
    _timer = Timer.periodic(const Duration(seconds: 1), (_) {
      _ticks++;
      if (_statuses.values.any((status) => status.retryAt != null)) {
        setState(() {});
      }
      if (_ticks % 2 == 0 &&
          (_statuses.values.any((status) => status.running) ||
              _ticks % 10 == 0)) {
        unawaited(_refresh());
      }
    });
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _refresh() async {
    if (_polling) return;
    _polling = true;
    final epoch = widget.store.profileEpoch;
    await _refreshRuntimeSources(epoch);
    try {
      await Future.wait([
        for (final source in widget.store.sources)
          (() async {
            final revision = _revisions[source.id] ?? 0;
            try {
              final status = await widget.repository.sourceStatus(source.id);
              if (!mounted ||
                  epoch != widget.store.profileEpoch ||
                  revision != (_revisions[source.id] ?? 0)) {
                return;
              }
              setState(() {
                _statuses[source.id] = status;
                _errors.remove(source.id);
              });
            } catch (error) {
              if (mounted &&
                  epoch == widget.store.profileEpoch &&
                  revision == (_revisions[source.id] ?? 0)) {
                setState(() => _errors[source.id] = error.toString());
              }
            }
          })(),
      ]);
    } finally {
      _polling = false;
    }
  }

  /// 拉取已导入的 LibreTV 站点与 drpy 脚本源，同步 SourceSite 运行时注册表。
  /// 原生核心 libretv.json / drpy.json 是权威来源；本方法同时承担注册表自愈
  /// （启动同步失败或其他页面导入后，下次轮询即恢复）。
  Future<void> _refreshRuntimeSources(int epoch) async {
    if (!widget.repository.supportsSourceManagement) return;
    var sites = _libretvSites;
    var scripts = _drpySites;
    var parseConfig = _parseConfig;
    try {
      final fetched = await widget.repository.libretvSites();
      if (!mounted || epoch != widget.store.profileEpoch) return;
      sites = fetched;
    } catch (_) {
      // 非原生环境或核心暂不可用时静默跳过，不影响其他站源状态展示。
    }
    try {
      final fetched = await widget.repository.drpySites();
      if (!mounted || epoch != widget.store.profileEpoch) return;
      scripts = fetched;
    } catch (_) {
      // 旧版核心没有 drpy 动作时静默跳过，LibreTV 片源不受影响。
    }
    try {
      final fetched = await widget.repository.libretvParseConfig();
      if (!mounted || epoch != widget.store.profileEpoch) return;
      parseConfig = fetched;
    } catch (_) {
      // 旧版核心没有解析方式动作时静默跳过，按「直链/嗅探」展示。
    }
    final keys = {
      for (final site in sites) site.sourceId,
      for (final script in scripts) script.sourceId,
    };
    final previous = {
      for (final site in _libretvSites) site.sourceId,
      for (final script in _drpySites) script.sourceId,
    };
    final changed =
        keys.length != previous.length ||
        keys.any((key) => !previous.contains(key));
    SourceSite.setRuntimeSources([
      for (final site in sites) site.toSourceSite(),
      for (final script in scripts) script.toSourceSite(),
    ]);
    setState(() {
      _libretvSites = sites;
      _drpySites = scripts;
      _parseConfig = parseConfig;
    });
    // 注册表发生变化才广播，避免每 10 秒轮询都惊动首页刷新。
    if (changed) widget.store.notifySourceRegistryChanged();
  }

  Future<void> _run(SourceSite source, String operation) async {
    if (_pending.contains(source.id)) return;
    final epoch = widget.store.profileEpoch;
    setState(() {
      _pending.add(source.id);
      _errors.remove(source.id);
      _revisions[source.id] = (_revisions[source.id] ?? 0) + 1;
      if (operation == 'check' || operation == 'checkCatalog') {
        _expandedHealth.add(source.id);
      }
    });
    try {
      final status = operation == 'cancel'
          ? await widget.repository.cancelSourceJob(source.id)
          : await widget.repository.startSourceJob(
              source.id,
              operation,
              drama: source.id == widget.drama?.source ? widget.drama : null,
            );
      if (mounted && epoch == widget.store.profileEpoch) {
        setState(() => _statuses[source.id] = status);
      }
    } catch (error) {
      if (mounted && epoch == widget.store.profileEpoch) {
        setState(() => _errors[source.id] = error.toString());
      }
    } finally {
      if (mounted) setState(() => _pending.remove(source.id));
    }
  }

  Future<void> _copy(SourceSite source, SourceStatus status) async {
    final text = StringBuffer('${source.name}\n');
    text.writeln(
      '缓存 ${status.count} 部，更新 ${sourceTimestamp(status.updatedAt)}',
    );
    final health = status.health;
    if (health != null) {
      text.writeln('${health.label} · ${sourceTimestamp(health.checkedAt)}');
      if (health.sample.isNotEmpty) text.writeln('检测剧集：${health.sample}');
      for (final step in health.steps) {
        text.writeln('${step.name}：${step.message}');
        text.writeln(
          '${step.host} HTTP ${step.httpStatus} · ${step.elapsedMs} ms',
        );
        if (step.cfRay.isNotEmpty) text.writeln('CF Ray: ${step.cfRay}');
      }
    }
    if (status.error.isNotEmpty) text.writeln(status.error);
    if (status.storageError.isNotEmpty) text.writeln(status.storageError);
    await Clipboard.setData(ClipboardData(text: text.toString()));
    if (mounted) {
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(const SnackBar(content: Text('诊断信息已复制')));
    }
  }

  @override
  Widget build(BuildContext context) => AnimatedBuilder(
    animation: widget.store,
    builder: (context, _) {
      final sources = widget.store.sources.toList()
        ..sort((a, b) {
          final aFirst = a.id == widget.initialSource ? 0 : 1;
          final bFirst = b.id == widget.initialSource ? 0 : 1;
          return aFirst.compareTo(bFirst);
        });
      final viewPaddingBottom = MediaQuery.viewPaddingOf(context).bottom;
      final paddingBottom = MediaQuery.paddingOf(context).bottom;
      final bottomInset = viewPaddingBottom > paddingBottom
          ? viewPaddingBottom
          : paddingBottom;
      return Scaffold(
        appBar: AppBar(title: const Text('站源管理')),
        body: Center(
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 960),
            child: RefreshIndicator(
              onRefresh: _refresh,
              child: ListView(
                padding: EdgeInsets.fromLTRB(16, 16, 16, 16 + bottomInset),
                children: [
                  const Padding(
                    padding: EdgeInsets.only(bottom: 16),
                    child: Text(
                      '各站源可分别更新和检测。更新会查找新剧、继续加载一页历史内容，并分批补齐资料；离开此页后任务继续。',
                    ),
                  ),
                  _libretvCard(),
                  if (sources.isEmpty)
                    const Padding(
                      padding: EdgeInsets.all(24),
                      child: Text('当前用户没有可用站源'),
                    ),
                  for (final group in SourceGroup.fromSources(sources))
                    if (group.id == 'huangguo')
                      Card(
                        clipBehavior: Clip.antiAlias,
                        child: ExpansionTile(
                          key: const PageStorageKey('source-group-huangguo'),
                          initiallyExpanded: group.sources.any(
                            (source) => source.id == widget.initialSource,
                          ),
                          leading: const Icon(Icons.hub_outlined),
                          title: const Text('黄果'),
                          subtitle: Text(
                            '${group.sources.length} 个入口 · ${group.sources.fold<int>(0, (count, source) => count + (_statuses[source.id]?.count ?? 0))} 部',
                          ),
                          childrenPadding: const EdgeInsets.all(8),
                          children: [
                            for (final source in group.sources)
                              _sourceCard(source),
                          ],
                        ),
                      )
                    else
                      for (final source in group.sources) _sourceCard(source),
                ],
              ),
            ),
          ),
        ),
      );
    },
  );

  /// LibreTV / 扩展片源导入入口卡片：说明支持的输入形态 + 已导入来源管理
  /// （LibreTV 片源与 drpy 脚本源同卡分两区）。
  Widget _libretvCard() {
    final theme = Theme.of(context);
    final sites = _libretvSites;
    final scripts = _drpySites;
    final supported = widget.repository.supportsSourceManagement;
    final total = sites.length + scripts.length;
    return Card(
      clipBehavior: Clip.antiAlias,
      margin: const EdgeInsets.only(bottom: 16),
      child: ExpansionTile(
        key: const PageStorageKey('libretv-import'),
        leading: const Icon(Icons.playlist_add_outlined),
        title: const Text('LibreTV / 扩展片源'),
        subtitle: Text(
          total == 0
              ? '导入 LibreTV / MacCMS 片源与 drpy 脚本源'
              : '已导入 $total 个来源（${sites.length} 片源 · ${scripts.length} 脚本）',
        ),
        childrenPadding: const EdgeInsets.fromLTRB(16, 0, 16, 16),
        children: [
          Text(
            '可粘贴四种内容：LibreTV 订阅配置链接（返回 {"sites":[...]} 的 JSON 地址）、'
            '配置 JSON 原文、社区工具导出的 Base58 订阅文本（LunaTV-config 的 format=2 / format=3），'
            '或单个 MacCMS provide/vod 采集地址。'
            '支持 type 0/1（JSON 采集）与 type 2（XML 采集）。'
            '也可以直接粘贴 drpy 脚本源（含 var rule 的 JS，或 .js 脚本地址），'
            '导入时会自动识别分流。',
            style: theme.textTheme.bodySmall,
          ),
          const SizedBox(height: 12),
          Align(
            alignment: Alignment.centerLeft,
            child: Tooltip(
              message: supported
                  ? '粘贴订阅链接、配置 JSON、Base58 订阅文本、MacCMS 采集地址或 drpy 脚本'
                  : '当前环境不支持导入片源',
              child: FilledButton.icon(
                onPressed: supported && !_libretvBusy ? _importLibreTV : null,
                icon: const Icon(Icons.add_link_rounded),
                label: Text(_libretvBusy ? '导入中…' : '导入片源 / 脚本'),
              ),
            ),
          ),
          if (_parseConfig.parses.isNotEmpty) ...[
            const Divider(height: 24),
            Align(
              alignment: Alignment.centerLeft,
              child: Text('播放解析方式', style: theme.textTheme.titleSmall),
            ),
            const SizedBox(height: 8),
            DropdownButtonFormField<String>(
              key: const ValueKey('libretv-parse-select'),
              initialValue: _parseConfig.selected,
              isExpanded: true,
              decoration: const InputDecoration(
                border: OutlineInputBorder(),
                isDense: true,
                helperMaxLines: 4,
                helperText:
                    '默认「直链/嗅探」不需要后端：直链直接播，播放页会尝试从网页里嗅探视频地址。'
                    '「JSON 接口」依赖第三方解析服务；「本地脚本」在本机执行，无后端；'
                    '「WebView 嗅探」用内置浏览器打开播放页并截获媒体请求（Android / iOS，暂不支持下载）。'
                    '仅对非直链播放页生效，直链采集源不受影响。',
              ),
              items: [
                const DropdownMenuItem(value: '', child: Text('直链 / 嗅探（默认）')),
                for (final parse in _parseConfig.parses)
                  DropdownMenuItem(
                    value: parse.url,
                    child: Text(
                      '${parse.name} · ${parse.kind}',
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                    ),
                  ),
              ],
              onChanged: supported && !_libretvBusy ? _selectParse : null,
            ),
            const SizedBox(height: 8),
          ],
          if (sites.isNotEmpty) ...[
            const Divider(height: 24),
            Align(
              alignment: Alignment.centerLeft,
              child: Text('LibreTV 片源', style: theme.textTheme.titleSmall),
            ),
            for (final site in sites)
              ListTile(
                contentPadding: EdgeInsets.zero,
                leading: const Icon(Icons.subscriptions_outlined),
                title: Text(site.name),
                subtitle: Text(
                  '${site.api}\n${site.typeLabel} · ${site.count} 部',
                ),
                isThreeLine: true,
                trailing: Row(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    IconButton(
                      tooltip: widget.store.isFavoriteSource(site.sourceId)
                          ? '取消收藏${site.name}'
                          : '收藏${site.name}',
                      onPressed: _libretvBusy
                          ? null
                          : () => _toggleFavoriteSource(
                              site.sourceId,
                              site.name,
                            ),
                      icon: Icon(
                        widget.store.isFavoriteSource(site.sourceId)
                            ? Icons.star_rounded
                            : Icons.star_border_rounded,
                      ),
                    ),
                    IconButton(
                      tooltip: widget.store.isHiddenSource(site.sourceId)
                          ? '恢复显示${site.name}'
                          : '隐藏${site.name}',
                      onPressed: _libretvBusy
                          ? null
                          : () => _toggleHiddenSource(
                              site.sourceId,
                              site.name,
                            ),
                      icon: Icon(
                        widget.store.isHiddenSource(site.sourceId)
                            ? Icons.visibility_off_outlined
                            : Icons.visibility_outlined,
                      ),
                    ),
                    IconButton(
                      tooltip: '删除${site.name}',
                      onPressed: _libretvBusy ? null : () => _removeLibreTV(site),
                      icon: const Icon(Icons.delete_outline),
                    ),
                  ],
                ),
              ),
          ],
          if (scripts.isNotEmpty) ...[
            const Divider(height: 24),
            Align(
              alignment: Alignment.centerLeft,
              child: Text('drpy 脚本源', style: theme.textTheme.titleSmall),
            ),
            for (final script in scripts)
              ListTile(
                contentPadding: EdgeInsets.zero,
                leading: const Icon(Icons.javascript_outlined),
                title: Row(
                  children: [
                    Flexible(child: Text(script.name)),
                    if (script.isCatVod) ...[
                      const SizedBox(width: 6),
                      // CatVod 蜘蛛依赖 lib/cat.js，标记一下便于区分方言。
                      Container(
                        padding: const EdgeInsets.symmetric(
                          horizontal: 6,
                          vertical: 1,
                        ),
                        decoration: BoxDecoration(
                          color: theme.colorScheme.secondaryContainer,
                          borderRadius: BorderRadius.circular(6),
                        ),
                        child: Text(
                          'CatVod',
                          style: theme.textTheme.labelSmall?.copyWith(
                            color: theme.colorScheme.onSecondaryContainer,
                          ),
                        ),
                      ),
                    ],
                  ],
                ),
                subtitle: Text(
                  '${script.detailLabel}${script.scriptUrl.isNotEmpty ? '\n${script.scriptUrl}' : ''}\n'
                  '${script.count} 部',
                ),
                isThreeLine: true,
                trailing: Row(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    IconButton(
                      tooltip: widget.store.isFavoriteSource(script.sourceId)
                          ? '取消收藏${script.name}'
                          : '收藏${script.name}',
                      onPressed: _libretvBusy
                          ? null
                          : () => _toggleFavoriteSource(
                              script.sourceId,
                              script.name,
                            ),
                      icon: Icon(
                        widget.store.isFavoriteSource(script.sourceId)
                            ? Icons.star_rounded
                            : Icons.star_border_rounded,
                      ),
                    ),
                    IconButton(
                      tooltip: widget.store.isHiddenSource(script.sourceId)
                          ? '恢复显示${script.name}'
                          : '隐藏${script.name}',
                      onPressed: _libretvBusy
                          ? null
                          : () => _toggleHiddenSource(
                              script.sourceId,
                              script.name,
                            ),
                      icon: Icon(
                        widget.store.isHiddenSource(script.sourceId)
                            ? Icons.visibility_off_outlined
                            : Icons.visibility_outlined,
                      ),
                    ),
                    IconButton(
                      tooltip: '删除${script.name}',
                      onPressed: _libretvBusy
                          ? null
                          : () => _removeLibreTVKey(script.sourceId, script.name),
                      icon: const Icon(Icons.delete_outline),
                    ),
                  ],
                ),
              ),
          ],
        ],
      ),
    );
  }

  Future<void> _importLibreTV() async {
    final input = await showDialog<String>(
      context: context,
      builder: (_) => const _LibreTVImportDialog(),
    );
    if (input == null || input.isEmpty) return;
    if (_looksLikeDrpyInput(input)) {
      await _importDrpy(input);
      return;
    }
    setState(() => _libretvBusy = true);
    try {
      final result = await widget.repository.libretvImport(input);
      if (!mounted) return;
      // 导入已入库：先用导入结果即时刷新（计数待校正），再异步拉完整列表。
      final merged = <LibreTVSite>[
        for (final site in _libretvSites)
          if (!result.added.any((added) => added.key == site.key)) site,
        ...result.added,
      ];
      SourceSite.setRuntimeSources([
        for (final site in merged) site.toSourceSite(),
        // 保留已导入的 drpy 脚本运行时源，避免导入片源时被清掉。
        for (final script in _drpySites) script.toSourceSite(),
      ]);
      setState(() {
        _libretvSites = merged;
        _libretvBusy = false;
      });
      widget.store.notifySourceRegistryChanged();
      final skipped = result.skipped;
      final summary = StringBuffer('新增 ${result.added.length} 个片源');
      summary.write(' · 已验证 ${result.verified} 个');
      if (result.parses.isNotEmpty) {
        summary.write(' · ${result.parses.length} 种解析方式');
      }
      if (skipped.isNotEmpty) {
        final preview = skipped.take(2).join('、');
        summary.write(
          ' · 跳过 ${skipped.length} 个（$preview${skipped.length > 2 ? ' 等' : ''}）',
        );
      }
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(SnackBar(content: Text(summary.toString())));
      // 混合订阅里发现的 drpy 脚本候选：弹框等用户确认后再逐个/一次性导入。
      if (result.drpy.isNotEmpty) {
        await showDialog<void>(
          context: context,
          builder: (_) => _DrpyCandidatesDialog(
            candidates: result.drpy,
            onImport: (candidate) => _importDrpyOrThrow(candidate.api),
          ),
        );
        if (!mounted) return;
      }
      try {
        final sites = await widget.repository.libretvSites();
        if (!mounted) return;
        SourceSite.setRuntimeSources([
          for (final site in sites) site.toSourceSite(),
          // 候选对话框可能刚导入 drpy 脚本源，这里不能只剩 libretv。
          for (final script in _drpySites) script.toSourceSite(),
        ]);
        setState(() => _libretvSites = sites);
      } catch (_) {
        // 计数校正失败不影响导入结果，下次轮询会补齐。
      }
      // 订阅里可能带新的解析规则：刷新一次，让下拉框立即可选。
      try {
        final parseConfig = await widget.repository.libretvParseConfig();
        if (!mounted) return;
        setState(() => _parseConfig = parseConfig);
      } catch (_) {
        // 旧版核心没有该动作时静默跳过。
      }
    } catch (error) {
      if (mounted) {
        setState(() => _libretvBusy = false);
        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('导入失败：$error')));
      }
    }
  }

  /// 输入是否更像 drpy 脚本：含 var/let/const rule 的 JS 文本，
  /// 或指向 .js 的 http(s) 地址（排除 provide/vod 采集接口）。
  bool _looksLikeDrpyInput(String input) {
    final text = input.trim();
    if (RegExp(r'\b(?:var|let|const)\s+rule\b').hasMatch(text)) return true;
    if (text.startsWith('http') &&
        RegExp(r'\.js(?:[?#]|$)').hasMatch(text) &&
        !text.contains('provide/vod')) {
      return true;
    }
    return false;
  }

  /// 切换 LibreTV 解析方式；空串 = 直链/嗅探。
  Future<void> _selectParse(String? url) async {
    if (url == null || _libretvBusy) return;
    if (url == _parseConfig.selected) return;
    setState(() => _libretvBusy = true);
    try {
      await widget.repository.libretvSelectParse(url);
      final config = await widget.repository.libretvParseConfig();
      if (!mounted) return;
      setState(() {
        _parseConfig = config;
        _libretvBusy = false;
      });
      var label = url.isEmpty ? '直链 / 嗅探' : '所选解析';
      for (final parse in _parseConfig.parses) {
        if (parse.url == url) {
          label = parse.name;
          break;
        }
      }
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(SnackBar(content: Text('解析方式已切换为「$label」')));
    } catch (error) {
      if (!mounted) return;
      setState(() => _libretvBusy = false);
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(SnackBar(content: Text('切换解析方式失败：$error')));
    }
  }

  /// 导入 drpy 脚本源：导入成功即刷新列表与运行时注册表。
  /// 失败时吞掉异常仅弹提示（保持旧行为）。
  Future<void> _importDrpy(String script) async {
    try {
      await _importDrpyOrThrow(script);
    } catch (error) {
      if (mounted) {
        setState(() => _libretvBusy = false);
        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('导入失败：$error')));
      }
    }
  }

  /// drpy 导入核心：成功返回，失败抛出（供候选对话框感知逐条结果）。
  Future<void> _importDrpyOrThrow(String script) async {
    setState(() => _libretvBusy = true);
    try {
      final result = await widget.repository.drpyImport(script);
      if (!mounted) return;
      final site = DrpySite(
        key: result.key,
        name: result.name,
        engine: result.engine,
      );
      final merged = <DrpySite>[
        for (final existing in _drpySites)
          if (existing.key != result.key) existing,
        site,
      ];
      SourceSite.setRuntimeSources([
        for (final existing in _libretvSites) existing.toSourceSite(),
        for (final script in merged) script.toSourceSite(),
      ]);
      setState(() {
        _drpySites = merged;
        _libretvBusy = false;
      });
      widget.store.notifySourceRegistryChanged();
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(SnackBar(content: Text('已导入脚本源「${result.name}」')));
      try {
        final scripts = await widget.repository.drpySites();
        if (!mounted) return;
        SourceSite.setRuntimeSources([
          for (final site in _libretvSites) site.toSourceSite(),
          for (final script in scripts) script.toSourceSite(),
        ]);
        setState(() => _drpySites = scripts);
      } catch (_) {
        // 计数校正失败不影响导入结果，下次轮询会补齐。
      }
    } catch (error) {
      if (mounted) setState(() => _libretvBusy = false);
      rethrow;
    }
  }

  Future<void> _removeLibreTV(LibreTVSite site) =>
      _removeLibreTVKey(site.sourceId, site.name);

  /// 收藏 / 取消收藏导入的片源：收藏后首页把它从 LibreTV / drpy 大组拆出
  /// 独占一组（与红果同级），不影响站源管理与更新检测。
  Future<void> _toggleFavoriteSource(String sourceId, String name) async {
    if (_libretvBusy) return;
    try {
      await widget.store.toggleFavoriteSource(sourceId);
      if (!mounted) return;
      final favorite = widget.store.isFavoriteSource(sourceId);
      setState(() {});
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(
            favorite ? '已收藏「$name」，首页将独立显示该片源' : '已取消收藏「$name」',
          ),
        ),
      );
    } catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('操作失败：$error')));
      }
    }
  }

  /// 隐藏 / 恢复导入的片源：与黄果等隐藏源同逻辑——隐藏后首页片源选择
  /// 不再显示，其追剧 / 历史内容一并过滤；在本页点同一按钮恢复。
  Future<void> _toggleHiddenSource(String sourceId, String name) async {
    if (_libretvBusy) return;
    try {
      await widget.store.toggleSourceHidden(sourceId);
      if (!mounted) return;
      final hidden = widget.store.isHiddenSource(sourceId);
      setState(() {});
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(
            hidden ? '已隐藏「$name」，可在本页恢复显示' : '已恢复显示「$name」',
          ),
        ),
      );
    } catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('操作失败：$error')));
      }
    }
  }

  /// 删除 LibreTV 片源或 drpy 脚本源：连同原生核心的目录缓存一并清除
  /// （Go 侧 removeLibreTVSite / removeDrpySite 负责），并同步注销运行时注册表。
  Future<void> _removeLibreTVKey(String sourceId, String name) async {
    final isDrpy = sourceId.startsWith('drpy:');
    final key = sourceId.startsWith('libretv:')
        ? sourceId.substring('libretv:'.length)
        : sourceId.startsWith('drpy:')
        ? sourceId.substring('drpy:'.length)
        : sourceId;
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: Text('删除「$name」？'),
        content: const Text('将同时清除该片源在本机的目录缓存，删除后需重新导入。'),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context, false),
            child: const Text('取消'),
          ),
          FilledButton(
            onPressed: () => Navigator.pop(context, true),
            child: const Text('删除'),
          ),
        ],
      ),
    );
    if (confirmed != true) return;
    setState(() => _libretvBusy = true);
    try {
      if (isDrpy) {
        await widget.repository.drpyRemove(key);
      } else {
        await widget.repository.libretvRemove(key);
      }
      if (!mounted) return;
      SourceSite.unregisterRuntime(sourceId);
      setState(() {
        if (isDrpy) {
          _drpySites = _drpySites
              .where((script) => script.sourceId != sourceId)
              .toList();
        } else {
          _libretvSites = _libretvSites
              .where((site) => site.sourceId != sourceId)
              .toList();
        }
        _libretvBusy = false;
      });
      widget.store.notifySourceRegistryChanged();
      ScaffoldMessenger.of(
        context,
      ).showSnackBar(SnackBar(content: Text('已删除「$name」')));
    } catch (error) {
      if (mounted) {
        setState(() => _libretvBusy = false);
        ScaffoldMessenger.of(
          context,
        ).showSnackBar(SnackBar(content: Text('删除失败：$error')));
      }
    }
  }

  Widget _sourceCard(SourceSite source) {
    final status = _statuses[source.id];
    final pending = _pending.contains(source.id);
    final busy = pending || status?.running == true;
    final seconds = status?.retrySeconds ?? 0;
    final enabled =
        !busy && seconds == 0 && widget.repository.supportsSourceManagement;
    final error = _errors[source.id] ?? status?.error ?? '';
    final health = status?.health;
    final healthExpanded = _expandedHealth.contains(source.id);
    final colors = Theme.of(context).colorScheme;
    return Card(
      key: ValueKey('source-${source.id}'),
      margin: const EdgeInsets.only(bottom: 16),
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                const Icon(Icons.dns_outlined),
                const SizedBox(width: 12),
                Expanded(
                  child: Text(
                    source.name,
                    style: Theme.of(context).textTheme.titleLarge,
                  ),
                ),
                Text('${status?.count ?? 0} 部'),
              ],
            ),
            const SizedBox(height: 8),
            Text('最近更新：${sourceTimestamp(status?.updatedAt)}'),
            if (status != null && status.count > 0)
              Text(
                '已加载至第 ${status.page} 页${status.hasMore ? ' · 可继续加载' : ' · 当前分页已加载完'}',
              ),
            const SizedBox(height: 12),
            Wrap(
              spacing: 10,
              runSpacing: 8,
              crossAxisAlignment: WrapCrossAlignment.center,
              children: [
                FilledButton.icon(
                  key: ValueKey('update-${source.id}'),
                  onPressed: enabled ? () => _run(source, 'update') : null,
                  icon: const Icon(Icons.sync_rounded),
                  label: const Text('更新'),
                ),
                OutlinedButton.icon(
                  key: ValueKey('check-${source.id}'),
                  onPressed: enabled ? () => _run(source, 'check') : null,
                  icon: const Icon(Icons.network_check),
                  label: const Text('检测连接与播放'),
                ),
                if (status?.running == true)
                  TextButton(
                    onPressed: pending ? null : () => _run(source, 'cancel'),
                    child: const Text('停止'),
                  ),
                PopupMenuButton<String>(
                  tooltip: '${source.name}更多操作',
                  enabled: enabled,
                  onSelected: (operation) {
                    if (operation == 'removeLibreTV') {
                      _removeLibreTVKey(source.id, source.name);
                      return;
                    }
                    _run(source, operation);
                  },
                  itemBuilder: (_) => [
                    PopupMenuItem(
                      value: 'more',
                      enabled: status?.hasMore ?? true,
                      child: const Text('继续加载一页'),
                    ),
                    const PopupMenuItem(value: 'metadata', child: Text('补齐资料')),
                    if (source.id == 'huangdou')
                      PopupMenuItem(
                        value: 'vipMetadata',
                        enabled: (status?.unknownVip ?? 0) > 0,
                        child: Text('补齐 VIP 资料（${status?.unknownVip ?? 0} 部）'),
                      ),
                    const PopupMenuItem(
                      value: 'checkCatalog',
                      child: Text('仅检测目录'),
                    ),
                    if (source.isRuntime)
                      PopupMenuItem(
                        value: 'removeLibreTV',
                        enabled: !_libretvBusy,
                        child: const Text('删除此片源'),
                      ),
                  ],
                ),
              ],
            ),
            if (busy) ...[
              const SizedBox(height: 12),
              LinearProgressIndicator(
                value: status != null && status.total > 0
                    ? (status.completed / status.total).clamp(0, 1)
                    : null,
              ),
              const SizedBox(height: 8),
              Text(
                '${status?.stage ?? '准备中'}${(status?.total ?? 0) > 0 ? ' · ${status!.completed}/${status.total}' : ''}',
              ),
            ] else if (status != null && status.stage.isNotEmpty) ...[
              const SizedBox(height: 12),
              Text(
                '${status.stage}${status.added > 0 ? ' · 新增 ${status.added} 部' : ''}',
              ),
            ],
            if (seconds > 0)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  '请在 $seconds 秒后重试',
                  style: TextStyle(color: colors.error),
                ),
              ),
            if (error.isNotEmpty)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: SelectableText(
                  error,
                  style: TextStyle(color: colors.error),
                ),
              ),
            if (status != null && status.storageError.isNotEmpty)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    Text(
                      status.storageError,
                      style: TextStyle(color: colors.error),
                    ),
                    TextButton.icon(
                      key: ValueKey('save-${source.id}'),
                      onPressed:
                          !busy && widget.repository.supportsSourceManagement
                          ? () => _run(source, 'retrySave')
                          : null,
                      icon: const Icon(Icons.save_outlined),
                      label: const Text('重试保存'),
                    ),
                  ],
                ),
              ),
            if (health != null) ...[
              const Divider(height: 28),
              Row(
                children: [
                  Expanded(
                    child: Semantics(
                      expanded: healthExpanded,
                      child: Tooltip(
                        message: healthExpanded ? '收起检测详情' : '展开检测详情',
                        child: TextButton(
                          key: ValueKey('health-toggle-${source.id}'),
                          onPressed: () => setState(() {
                            if (healthExpanded) {
                              _expandedHealth.remove(source.id);
                            } else {
                              _expandedHealth.add(source.id);
                            }
                          }),
                          style: TextButton.styleFrom(
                            foregroundColor: colors.onSurface,
                            padding: const EdgeInsets.symmetric(
                              horizontal: 4,
                              vertical: 8,
                            ),
                          ),
                          child: Row(
                            children: [
                              Expanded(
                                child: Column(
                                  crossAxisAlignment: CrossAxisAlignment.start,
                                  children: [
                                    Text(health.label),
                                    Text(
                                      sourceTimestamp(health.checkedAt),
                                      style: Theme.of(
                                        context,
                                      ).textTheme.bodySmall,
                                    ),
                                  ],
                                ),
                              ),
                              const SizedBox(width: 8),
                              Icon(
                                healthExpanded
                                    ? Icons.expand_less_rounded
                                    : Icons.expand_more_rounded,
                              ),
                            ],
                          ),
                        ),
                      ),
                    ),
                  ),
                  IconButton(
                    tooltip: '复制诊断信息',
                    onPressed: () => _copy(source, status!),
                    icon: const Icon(Icons.copy_rounded),
                  ),
                ],
              ),
              if (healthExpanded) ...[
                if (health.sample.isNotEmpty) Text('检测剧集：${health.sample}'),
                for (final step in health.steps)
                  Padding(
                    padding: const EdgeInsets.only(top: 10),
                    child: Row(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Icon(
                          step.state == 'ok'
                              ? Icons.check_circle_outline
                              : Icons.error_outline,
                          size: 20,
                          color: step.state == 'ok'
                              ? colors.primary
                              : colors.error,
                        ),
                        const SizedBox(width: 8),
                        Expanded(
                          child: Column(
                            crossAxisAlignment: CrossAxisAlignment.start,
                            children: [
                              Text('${step.name}：${step.message}'),
                              if (step.host.isNotEmpty || step.httpStatus > 0)
                                Text(
                                  '${step.host}${step.httpStatus > 0 ? ' · HTTP ${step.httpStatus}' : ''} · ${step.elapsedMs} ms',
                                  style: Theme.of(context).textTheme.bodySmall,
                                ),
                            ],
                          ),
                        ),
                      ],
                    ),
                  ),
              ],
            ] else ...[
              const SizedBox(height: 12),
              const Text('尚未检测连接'),
            ],
          ],
        ),
      ),
    );
  }
}

/// LibreTV 导入对话框。TextField 的 controller 由对话框自己的 State
/// 持有并在 dispose 时释放——放在调用方会在对话框退场动画期间提前
/// dispose，触发「controller used after disposed」断言。
class _LibreTVImportDialog extends StatefulWidget {
  const _LibreTVImportDialog();

  @override
  State<_LibreTVImportDialog> createState() => _LibreTVImportDialogState();
}

class _LibreTVImportDialogState extends State<_LibreTVImportDialog> {
  final _controller = TextEditingController();

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
    title: const Text('导入 LibreTV 片源 / drpy 脚本'),
    content: TextField(
      controller: _controller,
      maxLines: 6,
      autofocus: true,
      decoration: const InputDecoration(
        labelText: '片源链接 / 配置 JSON / Base58 订阅 / 采集地址 / drpy 脚本',
        hintText:
            'https://example.com/libretv/config.json\n'
            '或 {"sites":[{"key":"a","name":"A","type":1,'
            '"api":"https://example.com/provide/vod"}]}\n'
            '或 LunaTV-config 导出的 Base58 订阅（format=2 / format=3）\n'
            '或 https://example.com/api.php/provide/vod/at/xml/\n'
            '或含 var rule 的 drpy 脚本（JS 原文或 .js 地址）\n'
            '或 CatVod 蜘蛛的 .js 地址（如 ./cat/xxx_open.js，需填完整地址）',
        border: OutlineInputBorder(),
      ),
    ),
    actions: [
      TextButton(
        onPressed: () => Navigator.pop(context),
        child: const Text('取消'),
      ),
      FilledButton(
        onPressed: () => Navigator.pop(context, _controller.text.trim()),
        child: const Text('导入'),
      ),
    ],
  );
}

/// 混合订阅里发现的 drpy 脚本候选确认框。
/// 脚本不会自动导入（需用户确认），这里提供逐个导入与全部导入。
class _DrpyCandidatesDialog extends StatefulWidget {
  const _DrpyCandidatesDialog({
    required this.candidates,
    required this.onImport,
  });

  final List<DrpyCandidate> candidates;

  /// 导入单个候选（由外部复用 drpy 导入链路，含列表刷新与提示）。
  final Future<void> Function(DrpyCandidate candidate) onImport;

  @override
  State<_DrpyCandidatesDialog> createState() => _DrpyCandidatesDialogState();
}

class _DrpyCandidatesDialogState extends State<_DrpyCandidatesDialog> {
  /// 已成功导入的候选地址集合（同一订阅内按 api 去重后不会重复）。
  final _imported = <String>{};
  bool _busy = false;
  String? _error;

  Future<void> _importOne(DrpyCandidate candidate) async {
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      await widget.onImport(candidate);
      if (!mounted) return;
      setState(() {
        _imported.add(candidate.api);
        _busy = false;
      });
    } catch (error) {
      if (!mounted) return;
      setState(() {
        _busy = false;
        _error = '「${candidate.name}」导入失败：$error';
      });
    }
  }

  Future<void> _importAll() async {
    setState(() {
      _busy = true;
      _error = null;
    });
    for (final candidate in widget.candidates) {
      if (_imported.contains(candidate.api)) continue;
      try {
        await widget.onImport(candidate);
        if (!mounted) return;
        setState(() => _imported.add(candidate.api));
      } catch (error) {
        if (!mounted) return;
        setState(() {
          _busy = false;
          _error = '「${candidate.name}」导入失败：$error';
        });
        return;
      }
    }
    if (!mounted) return;
    setState(() => _busy = false);
  }

  @override
  Widget build(BuildContext context) {
    final pending = widget.candidates
        .where((candidate) => !_imported.contains(candidate.api))
        .length;
    return AlertDialog(
      title: const Text('发现 drpy 脚本候选'),
      content: SizedBox(
        width: double.maxFinite,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              '订阅中包含 ${widget.candidates.length} 个 drpy 脚本'
              '（type≥3），需要确认后才会导入。',
            ),
            if (_error != null) ...[
              const SizedBox(height: 8),
              Text(
                _error!,
                style: TextStyle(color: Theme.of(context).colorScheme.error),
              ),
            ],
            const SizedBox(height: 8),
            Flexible(
              child: ListView.builder(
                shrinkWrap: true,
                itemCount: widget.candidates.length,
                itemBuilder: (context, index) {
                  final candidate = widget.candidates[index];
                  final done = _imported.contains(candidate.api);
                  return ListTile(
                    dense: true,
                    leading: Icon(
                      done ? Icons.check_circle : Icons.javascript_outlined,
                      color: done
                          ? Theme.of(context).colorScheme.primary
                          : null,
                    ),
                    title: Text(
                      candidate.name.isEmpty ? '未命名脚本' : candidate.name,
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                    ),
                    subtitle: Text(
                      candidate.api,
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                    ),
                    trailing: done
                        ? const Text('已导入')
                        : TextButton(
                            onPressed: _busy
                                ? null
                                : () => _importOne(candidate),
                            child: const Text('导入'),
                          ),
                  );
                },
              ),
            ),
          ],
        ),
      ),
      actions: [
        TextButton(
          onPressed: _busy ? null : () => Navigator.pop(context),
          child: Text(pending == 0 ? '完成' : '暂不导入'),
        ),
        FilledButton(
          onPressed: _busy || pending == 0 ? null : _importAll,
          child: Text(_busy ? '导入中…' : '全部导入'),
        ),
      ],
    );
  }
}

class SourceDiagnosticsButton extends StatelessWidget {
  const SourceDiagnosticsButton({
    super.key,
    required this.repository,
    required this.store,
    required this.drama,
  });
  final AppRepository repository;
  final LocalStore store;
  final Drama drama;

  @override
  Widget build(BuildContext context) => TextButton.icon(
    onPressed: () => Navigator.push<void>(
      context,
      MaterialPageRoute(
        builder: (_) => SourcesScreen(
          repository: repository,
          store: store,
          initialSource: drama.source,
          drama: drama,
        ),
      ),
    ),
    icon: const Icon(Icons.network_check),
    label: const Text('站源诊断'),
  );
}
