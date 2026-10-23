import 'package:duanju_app/source_health.dart';
import 'package:flutter_test/flutter_test.dart';

/// 源级健康度账本的规则回归。
///
/// 它只干两件事：把慢/死的源排到队尾、在冷却期里暂时跳过。任何一条规则
/// 走偏都会直接表现为「某个源莫名其妙不见了」或「死源继续拖慢刷新」，
/// 所以这里把边界钉死。
void main() {
  test('排序：健康（快→慢）→ 未知 → 失败（失败多→少）', () {
    final book = SourceHealthBook();
    book.record('slow', ok: true, elapsedMs: 900);
    book.record('fast', ok: true, elapsedMs: 100);
    book.record('dead', ok: false, elapsedMs: 5000);
    book.record('dead', ok: false, elapsedMs: 5000);
    book.record('flaky', ok: false, elapsedMs: 3000);

    expect(book.order(['dead', 'unknown', 'slow', 'flaky', 'fast']), [
      'fast',
      'slow',
      'unknown',
      'dead',
      'flaky',
    ]);
  });

  test('同档内保持调用方给的原始顺序（排序稳定）', () {
    final book = SourceHealthBook();
    for (final id in ['c', 'a', 'b', 'd', 'e']) {
      book.record(id, ok: true, elapsedMs: 200);
    }
    expect(book.order(['c', 'a', 'b', 'd', 'e']), ['c', 'a', 'b', 'd', 'e']);
    expect(book.order(['e', 'd', 'c', 'b', 'a']), ['e', 'd', 'c', 'b', 'a']);
  });

  test('连续失败 >= 2 才跳过，成功一次立刻恢复', () {
    final book = SourceHealthBook();
    book.record('a', ok: false, elapsedMs: 0);
    expect(book.shouldSkip('a'), isFalse, reason: '偶发一次失败不该跳过');

    book.record('a', ok: false, elapsedMs: 0);
    expect(book.shouldSkip('a'), isTrue);

    book.record('a', ok: true, elapsedMs: 120);
    expect(book.shouldSkip('a'), isFalse);
    expect(book.healthOf('a')!.failures, 0);
  });

  test('冷却到期自动重试；force 立即重试', () {
    var now = DateTime(2026, 1, 1, 12);
    final book = SourceHealthBook(now: () => now);
    book.record('a', ok: false, elapsedMs: 0);
    book.record('a', ok: false, elapsedMs: 0);
    expect(book.shouldSkip('a'), isTrue);

    now = now.add(SourceHealthBook.cooldown - const Duration(seconds: 1));
    expect(book.shouldSkip('a'), isTrue);
    now = now.add(const Duration(seconds: 2));
    expect(book.shouldSkip('a'), isFalse);

    book.record('b', ok: false, elapsedMs: 0);
    book.record('b', ok: false, elapsedMs: 0);
    final plan = book.plan(['a', 'b'], force: true);
    expect(plan.skipped, isEmpty);
    expect(plan.order.length, 2);
  });

  test('plan 同时给出「保留顺序」与「跳过名单」', () {
    final book = SourceHealthBook();
    book.record('dead', ok: false, elapsedMs: 0);
    book.record('dead', ok: false, elapsedMs: 0);
    final plan = book.plan(['dead', 'live']);
    expect(plan.skipped, ['dead']);
    expect(plan.order, ['live']);
  });

  test('时钟回拨不会把源永久锁死', () {
    var now = DateTime(2026, 1, 1, 12);
    final book = SourceHealthBook(now: () => now);
    book.record('a', ok: false, elapsedMs: 0);
    book.record('a', ok: false, elapsedMs: 0);
    now = now.subtract(const Duration(hours: 2));
    expect(book.shouldSkip('a'), isFalse);
  });

  test('平均耗时用滑动平均，成功会重置失败计数', () {
    final book = SourceHealthBook();
    book.record('a', ok: true, elapsedMs: 1000);
    book.record('a', ok: true, elapsedMs: 0);
    final avg = book.healthOf('a')!.avgMs;
    expect(avg, closeTo(700, 0.001));
    expect(book.healthOf('a')!.samples, 2);
  });

  test('JSON 往返；损坏数据当空账本而不是抛错', () {
    final book = SourceHealthBook();
    book.record('a', ok: true, elapsedMs: 120);
    book.record('b', ok: false, elapsedMs: 0);
    book.record('b', ok: false, elapsedMs: 0);

    final restored = SourceHealthBook.fromJson(book.toJson());
    expect(restored.healthOf('a')!.avgMs, 120);
    expect(restored.healthOf('b')!.failures, 2);
    expect(restored.shouldSkip('b'), isTrue);

    expect(SourceHealthBook.fromJson('{not json').isEmpty, isTrue);
    expect(SourceHealthBook.fromJson('{"sources":"x"}').isEmpty, isTrue);
    expect(SourceHealthBook.fromJson('').isEmpty, isTrue);
  });

  test('账本条目有上限，避免删了又加留下无限孤儿记录', () {
    final book = SourceHealthBook();
    for (var index = 0; index < SourceHealthBook.maxEntries + 50; index++) {
      book.record('src-$index', ok: true, elapsedMs: index);
    }
    expect(book.length, SourceHealthBook.maxEntries);
  });

  test('reset 清掉单个源的记录', () {
    final book = SourceHealthBook();
    book.record('a', ok: false, elapsedMs: 0);
    book.record('a', ok: false, elapsedMs: 0);
    book.reset('a');
    expect(book.shouldSkip('a'), isFalse);
    expect(book.healthOf('a'), isNull);
  });
}
