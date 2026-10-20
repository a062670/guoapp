import 'package:duanju_app/models.dart';
import 'package:duanju_app/playback_route_memory.dart';
import 'package:flutter_test/flutter_test.dart';

Drama drama(String source, String id) =>
    Drama(id: id, source: source, title: '测试剧 $id', episodes: 12);

void main() {
  test('remembers the route that played and falls back to zero', () {
    final memory = PlaybackRouteMemory();
    final show = drama('huangdou', 'huangdou:100');
    expect(memory.routeOf(show), 0);

    memory.remember(show, 2);
    expect(memory.routeOf(show), 2);

    // 首选线路不需要记（0 是默认值，记了也不会更快）。
    memory.remember(show, 0);
    expect(memory.routeOf(show), 0);
    expect(memory.length, 0);
  });

  test('routes are scoped per source and drama', () {
    final memory = PlaybackRouteMemory();
    final first = drama('huangdou', 'huangdou:100');
    final otherSource = drama('guipian', 'huangdou:100');
    final otherShow = drama('huangdou', 'huangdou:200');
    memory.remember(first, 1);
    expect(memory.routeOf(otherSource), 0);
    expect(memory.routeOf(otherShow), 0);
  });

  test('forget drops a broken route and clear empties the memory', () {
    final memory = PlaybackRouteMemory();
    final show = drama('huangdou', 'huangdou:100');
    memory.remember(show, 3);
    memory.forget(show);
    expect(memory.routeOf(show), 0);
    memory.remember(show, 2);
    memory.clear();
    expect(memory.length, 0);
  });

  test('the least recently inserted route is evicted past the limit', () {
    final memory = PlaybackRouteMemory(limit: 2);
    final first = drama('huangdou', 'huangdou:1');
    final second = drama('huangdou', 'huangdou:2');
    final third = drama('huangdou', 'huangdou:3');
    memory.remember(first, 1);
    memory.remember(second, 1);
    memory.remember(third, 1);
    expect(memory.length, 2);
    expect(memory.routeOf(first), 0);
    expect(memory.routeOf(second), 1);
    expect(memory.routeOf(third), 1);
  });

  test('a remembered route expires after the ttl', () {
    var now = DateTime(2026, 10, 19, 20);
    final memory = PlaybackRouteMemory(now: () => now);
    final show = drama('huangdou', 'huangdou:100');
    memory.remember(show, 2);
    expect(memory.routeOf(show), 2);

    // 14 分钟内仍然有效。
    now = now.add(const Duration(minutes: 14));
    expect(memory.routeOf(show), 2);

    // 超过 15 分钟：回到默认线路重新试，并清掉记录。
    now = now.add(const Duration(minutes: 2));
    expect(memory.routeOf(show), 0, reason: '过期后应回到默认线路');
    expect(memory.length, 0);
  });

  test('a plan carries the preload verification flag', () {
    final plan = PlaybackPlan.fromJson(<String, dynamic>{
      'url': 'https://play.test/a.m3u8',
      'session': 'session-token',
      'routeIndex': 1,
      'routeCount': 3,
      'verified': true,
    });
    expect(plan.verified, isTrue);
    expect(plan.hasAlternative, isTrue);
    final plain = PlaybackPlan.fromJson(<String, dynamic>{
      'url': 'https://play.test/a.m3u8',
      'session': 's',
      'routeIndex': 0,
      'routeCount': 1,
    });
    expect(plain.verified, isFalse);
    expect(plain.hasAlternative, isFalse);
  });

  test('the last route still has an alternative (wraps to the first)', () {
    final plan = PlaybackPlan.fromJson(<String, dynamic>{
      'url': 'https://play.test/a.m3u8',
      'session': 's',
      'routeIndex': 2,
      'routeCount': 3,
    });
    expect(plan.hasAlternative, isTrue, reason: '环形回退：最后一条线路也能切回首选线路');
  });
}
