import 'dart:async';

import 'package:duanju_app/cover_request_gate.dart';
import 'package:flutter_test/flutter_test.dart';

/// 封面请求闸门：并发有上限、同图合流、失败后可以重试。
void main() {
  test('并发不超过上限，超出的按 FIFO 排队', () async {
    final gate = CoverRequestGate(concurrency: 4);
    final starters = <String>[];
    final releases = <String, Completer<void>>{};
    var peak = 0;

    Future<String> request(String key) => gate.load(key, () async {
      starters.add(key);
      peak = gate.active > peak ? gate.active : peak;
      final release = Completer<void>();
      releases[key] = release;
      await release.future;
      return key;
    });

    final futures = [for (var i = 0; i < 10; i++) request('k$i')];
    await Future<void>.delayed(Duration.zero);
    expect(gate.active, 4);
    expect(starters, ['k0', 'k1', 'k2', 'k3']);

    // 释放一个 → 下一个立刻补位。
    releases['k0']!.complete();
    await Future<void>.delayed(Duration.zero);
    expect(starters, ['k0', 'k1', 'k2', 'k3', 'k4']);

    for (final key in releases.keys.toList()) {
      if (!releases[key]!.isCompleted) releases[key]!.complete();
    }
    // 完成过程中会继续补位，逐个放行直到全部完成。
    for (var i = 0; i < 20; i++) {
      await Future<void>.delayed(Duration.zero);
      for (final key in releases.keys.toList()) {
        if (!releases[key]!.isCompleted) releases[key]!.complete();
      }
    }
    await Future.wait(futures);
    expect(peak, lessThanOrEqualTo(4));
    expect(starters.length, 10);
    expect(gate.active, 0);
  });

  test('同一个 key 只请求一次，结果共享', () async {
    final gate = CoverRequestGate(concurrency: 4);
    var calls = 0;
    final release = Completer<void>();
    Future<String> request() => gate.load('same', () async {
      calls++;
      await release.future;
      return '/tmp/same.jpg';
    });

    final first = request();
    final second = request();
    release.complete();
    expect(await first, '/tmp/same.jpg');
    expect(await second, '/tmp/same.jpg');
    expect(calls, 1);
  });

  test('失败会传播给等待者，且之后可以重新请求', () async {
    final gate = CoverRequestGate();
    var calls = 0;
    Future<String> request() => gate.load('bad', () async {
      calls++;
      if (calls == 1) throw StateError('海报暂不可用');
      return '/tmp/ok.jpg';
    });

    await expectLater(request(), throwsStateError);
    // 失败后 key 已释放（重试路径依赖这一点）。
    expect(await request(), '/tmp/ok.jpg');
    expect(calls, 2);
  });
}
