import 'dart:math';

import 'package:flutter/material.dart';

import 'app_layout.dart';
import 'models.dart';
import 'pinyin_sort.dart';
import 'remote_widgets.dart';

/// 分类标签栏的下拉手势判定结果。
enum CatalogPullAction {
  /// 未达到任一阈值：忽略。
  none,

  /// 轻拉：刷新当前片源目录（等价手动刷新）。
  refresh,

  /// 重拉：展开「分类面板」（大半屏，可排序选择分类）。
  expand,
}

/// 下拉手势阈值判定（纯函数，便于测试）。
///
/// - [distance] 本次向下拖动的累计像素（向下为正）；
/// - [velocity] 松手时的向下速度（px/s，向下为正）。
///
/// 轻拉刷新、重拉展开：两个阈值之间留给「只是想刷新一下」的轻拖，
/// 超过展开阈值（或甩动速度足够快）才打开面板，避免误触。
CatalogPullAction catalogPullAction(double distance, double velocity) {
  if (distance >= 90 || velocity >= 900) return CatalogPullAction.expand;
  if (distance >= 18 || velocity >= 240) return CatalogPullAction.refresh;
  return CatalogPullAction.none;
}

/// 「分类面板」的排序方式。
enum CatalogCategorySort {
  /// 站源返回的原始顺序。
  original,

  /// 按名称排序：中文按拼音首字母，英文 / 数字按字母（A-Z）。
  alphabetical,

  /// 按名称倒序（Unicode 码位）。
  reverse,
}

const _sortLabels = <CatalogCategorySort, String>{
  CatalogCategorySort.original: '默认顺序',
  CatalogCategorySort.alphabetical: '字母排序',
  CatalogCategorySort.reverse: '名称倒序',
};

/// 按排序方式重排分类列表（纯函数，便于测试）。
List<CatalogCategory> sortCategories(
  List<CatalogCategory> categories,
  CatalogCategorySort sort,
) {
  final list = List.of(categories);
  switch (sort) {
    case CatalogCategorySort.original:
      return list;
    case CatalogCategorySort.alphabetical:
      // 中文按拼音首字母、英文按字母：预计算排序键，键相同再按原名，
      // 保证结果稳定。
      final keyed = [
        for (final entry in list) (entry, PinyinSort.sortKey(entry.name)),
      ];
      keyed.sort((a, b) {
        final byKey = a.$2.compareTo(b.$2);
        return byKey != 0 ? byKey : a.$1.name.compareTo(b.$1.name);
      });
      return [for (final (entry, _) in keyed) entry];
    case CatalogCategorySort.reverse:
      list.sort((a, b) => b.name.compareTo(a.name));
      return list;
  }
}

class CatalogFilters extends StatefulWidget {
  const CatalogFilters({
    super.key,
    required this.categories,
    required this.category,
    required this.onCategory,
    required this.onRetry,
    this.error,
    this.trailing,
    this.onRefresh,
  });

  final List<CatalogCategory> categories;
  final String category;
  final String? error;
  final ValueChanged<String> onCategory;
  final VoidCallback onRetry;

  /// 标签栏右侧附加按钮（首页的 VIP 过滤等）。
  final Widget? trailing;

  /// 轻拉标签栏时的刷新回调（重新拉取当前分类目录）。
  final VoidCallback? onRefresh;

  @override
  State<CatalogFilters> createState() => _CatalogFiltersState();
}

class _CatalogFiltersState extends State<CatalogFilters> {
  final _anchors = <String, GlobalKey>{};

  /// 本次向下拖动的累计像素（向下为正），松手时按阈值判定动作。
  double _pullDistance = 0;

  @override
  void didUpdateWidget(covariant CatalogFilters oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.category != widget.category) {
      WidgetsBinding.instance.addPostFrameCallback((_) {
        final anchor = _anchors[widget.category]?.currentContext;
        if (mounted && anchor != null) {
          Scrollable.ensureVisible(
            anchor,
            alignment: .4,
            duration: const Duration(milliseconds: 180),
          );
        }
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    final television = AppLayout.isTelevision(context);
    return SizedBox(
      height: television
          ? 64
          : max(52, MediaQuery.textScalerOf(context).scale(14) + 28),
      // 下拉手势：轻拉刷新、重拉展开「分类面板」。
      // 分类 chips 自身是横向滚动，纵向拖动不会与它抢手势。
      child: GestureDetector(
        behavior: HitTestBehavior.translucent,
        onVerticalDragStart: (_) => _pullDistance = 0,
        onVerticalDragUpdate: (details) {
          final delta = details.primaryDelta ?? 0;
          if (delta > 0) _pullDistance += delta;
        },
        onVerticalDragEnd: (details) {
          final action = catalogPullAction(
            _pullDistance,
            details.primaryVelocity ?? 0,
          );
          _pullDistance = 0;
          switch (action) {
            case CatalogPullAction.none:
              break;
            case CatalogPullAction.refresh:
              widget.onRefresh?.call();
            case CatalogPullAction.expand:
              _openCategoryPanel();
          }
        },
        child: Row(
          children: [
            Expanded(
              child: SingleChildScrollView(
                key: const ValueKey('catalog-categories'),
                scrollDirection: Axis.horizontal,
                padding: const EdgeInsets.symmetric(horizontal: 12),
                child: Row(
                  children: [
                    for (final entry in widget.categories)
                      Padding(
                        key: _anchors.putIfAbsent(entry.id, GlobalKey.new),
                        padding: const EdgeInsets.only(right: 6),
                        child: television
                            ? RemoteButton(
                                key: ValueKey('category-${entry.id}'),
                                label: entry.name,
                                selected: entry.id == widget.category,
                                onPressed: () => widget.onCategory(entry.id),
                              )
                            : ChoiceChip(
                                key: ValueKey('category-${entry.id}'),
                                label: Text(entry.name),
                                selected: entry.id == widget.category,
                                showCheckmark: false,
                                onSelected: (_) => widget.onCategory(entry.id),
                              ),
                      ),
                  ],
                ),
              ),
            ),
            if (widget.error != null)
              IconButton(
                tooltip: widget.error,
                onPressed: widget.onRetry,
                icon: Icon(
                  Icons.refresh_rounded,
                  color: Theme.of(context).colorScheme.error,
                ),
              ),
            if (widget.trailing != null) widget.trailing!,
            // 「分类面板」按钮：点击与重拉等效，打开大半屏分类面板
            // （可排序、可快速跳分类），触屏与遥控都能用。
            IconButton(
              key: const ValueKey('catalog-panel-open'),
              tooltip: '分类面板',
              onPressed: _openCategoryPanel,
              icon: const Icon(Icons.expand_more_rounded),
            ),
          ],
        ),
      ),
    );
  }

  /// 打开大半屏「分类面板」：全部分类 + 排序切换 + 当前分类高亮。
  Future<void> _openCategoryPanel() async {
    await showModalBottomSheet<void>(
      context: context,
      isScrollControlled: true,
      isDismissible: true,
      enableDrag: true,
      // 大半屏（不抢满全屏，留出顶部片源上下文）。
      constraints: BoxConstraints(
        maxHeight: MediaQuery.sizeOf(context).height * 0.88,
      ),
      builder: (sheetContext) => _CategoryPanel(
        categories: widget.categories,
        category: widget.category,
        onCategory: widget.onCategory,
      ),
    );
  }
}

/// 「分类面板」内容：排序切换 + 全部分类 chips。
class _CategoryPanel extends StatefulWidget {
  const _CategoryPanel({
    required this.categories,
    required this.category,
    required this.onCategory,
  });

  final List<CatalogCategory> categories;
  final String category;
  final ValueChanged<String> onCategory;

  @override
  State<_CategoryPanel> createState() => _CategoryPanelState();
}

class _CategoryPanelState extends State<_CategoryPanel> {
  CatalogCategorySort _sort = CatalogCategorySort.original;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final categories = sortCategories(widget.categories, _sort);
    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 8, 16, 16),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Center(
              child: Container(
                width: 36,
                height: 4,
                decoration: BoxDecoration(
                  color: theme.colorScheme.onSurfaceVariant.withValues(
                    alpha: .4,
                  ),
                  borderRadius: BorderRadius.circular(2),
                ),
              ),
            ),
            const SizedBox(height: 12),
            Row(
              children: [
                Text('分类面板', style: theme.textTheme.titleMedium),
                const Spacer(),
                TextButton(
                  key: const ValueKey('catalog-panel-close'),
                  onPressed: () => Navigator.pop(context),
                  child: const Text('关闭'),
                ),
              ],
            ),
            const SizedBox(height: 4),
            Text(
              '向下轻拉标签栏可刷新，继续下拉打开本面板；也可点标签栏右侧的展开按钮。',
              style: theme.textTheme.bodySmall?.copyWith(
                color: theme.colorScheme.onSurfaceVariant,
              ),
            ),
            const SizedBox(height: 12),
            Text('排序', style: theme.textTheme.labelLarge),
            const SizedBox(height: 8),
            Wrap(
              spacing: 8,
              children: [
                for (final entry in _sortLabels.entries)
                  ChoiceChip(
                    key: ValueKey('catalog-panel-sort-${entry.key.name}'),
                    label: Text(entry.value),
                    selected: _sort == entry.key,
                    onSelected: (_) => setState(() => _sort = entry.key),
                  ),
              ],
            ),
            const Divider(height: 24),
            Expanded(
              child: SingleChildScrollView(
                child: Wrap(
                  spacing: 8,
                  runSpacing: 8,
                  children: [
                    for (final entry in categories)
                      ChoiceChip(
                        key: ValueKey('catalog-panel-${entry.id}'),
                        label: Text(entry.name),
                        selected: entry.id == widget.category,
                        showCheckmark: false,
                        onSelected: (_) {
                          widget.onCategory(entry.id);
                          Navigator.pop(context);
                        },
                      ),
                  ],
                ),
              ),
            ),
          ],
        ),
      ),
    );
  }
}
