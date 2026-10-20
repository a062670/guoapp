import 'package:flutter/gestures.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:media_kit/media_kit.dart';

import 'package:duanju_app/player_interactions.dart';

import 'player_fixtures.dart';

// 回归背景（2026-10-18）：旧实现在 pointerDown 命中左 35% 时就立即
// _showHud(brightness)，导致左半屏「长按倍速」「双击暂停」都会先闪一个
// 亮度图标。现在改为「确认垂直拖动超过阈值后才 latch 并显示」。
//
// 本文件用手造 Pointer 事件直接驱动 PlayerInteractions 的状态机，
// 不经过 GestureDetector（它内部还有 tap/双击识别，会引入额外时序）。
void main() {
  PointerDownEvent down(double x, double y, {int pointer = 1}) =>
      PointerDownEvent(
        pointer: pointer,
        buttons: kPrimaryButton,
        kind: PointerDeviceKind.touch,
        position: Offset(x, y),
      );

  PointerMoveEvent move(double x, double y, {int pointer = 1}) =>
      PointerMoveEvent(
        pointer: pointer,
        buttons: kPrimaryButton,
        kind: PointerDeviceKind.touch,
        position: Offset(x, y),
      );

  PointerUpEvent up(double x, double y, {int pointer = 1}) => PointerUpEvent(
    pointer: pointer,
    kind: PointerDeviceKind.touch,
    position: Offset(x, y),
  );

  Future<PlayerInteractions> makeInteractions(WidgetTester tester) async {
    final platform = ScriptedPlayer();
    await platform.open(Media('https://media.test/clip.mp4'));
    final player = Player(platformPlayer: platform);
    addTearDown(player.dispose);
    final interactions = PlayerInteractions(
      player: player,
      available: () => true,
      baseSpeed: () => 1.0,
      holdSpeed: () => 3.0,
      onTogglePlayback: () {},
      onFullscreen: () {},
      onEpisode: (direction) => direction > 0 ? '下一集' : '上一集',
    );
    addTearDown(interactions.dispose);
    await tester.pump(); // 让 getBrightness 的 future 走完
    return interactions;
  }

  // 视口 400×600：左 35% = x<140；亮度阈值 = max(30, min(80, 600*0.08)) = 48px
  const viewWidth = 400.0;
  const viewHeight = 600.0;

  testWidgets('长按（左半屏静止）进入倍速且不弹亮度 HUD', (tester) async {
    final interactions = await makeInteractions(tester);
    interactions.pointerDown(
      down(60, 300),
      swipeEnabled: true,
      width: viewWidth,
      height: viewHeight,
    );
    await tester.pump(const Duration(milliseconds: 400)); // 越过 350ms hold

    expect(interactions.boosting, isTrue, reason: '长按应进入倍速');
    expect(
      interactions.hudState.type,
      SwipeAction.none,
      reason: '静止长按不应显示亮度 HUD',
    );

    interactions.pointerUp(up(60, 300));
    await tester.pump(const Duration(seconds: 2)); // 冲掉恢复提示的 1200ms timer
    expect(interactions.boosting, isFalse);
  });

  testWidgets('双击（左半屏两次点按）不弹亮度 HUD', (tester) async {
    final interactions = await makeInteractions(tester);
    for (var i = 0; i < 2; i++) {
      interactions.pointerDown(
        down(60, 300, pointer: i + 1),
        swipeEnabled: true,
        width: viewWidth,
        height: viewHeight,
      );
      interactions.pointerUp(up(60, 300, pointer: i + 1));
    }
    await tester.pump();

    expect(
      interactions.hudState.type,
      SwipeAction.none,
      reason: '双击暂停不应显示亮度 HUD',
    );
  });

  testWidgets('左半屏垂直拖动超过阈值才显示 HUD 并调节亮度', (tester) async {
    final interactions = await makeInteractions(tester);
    interactions.pointerDown(
      down(60, 400),
      swipeEnabled: true,
      width: viewWidth,
      height: viewHeight,
    );

    // 轻微移动（12~48px 之间）：不 latch、不改亮度
    interactions.pointerMove(move(62, 370)); // dy=30 < 48
    expect(interactions.hudState.type, SwipeAction.none, reason: '未超过阈值不应显示亮度');
    expect(interactions.brightness, 0.5, reason: '未确认前不应改变亮度');

    // 垂直上滑 100px：latch 并显示（latch 当帧只定基准，亮度不变）
    interactions.pointerMove(move(60, 300)); // dy=100 > 48，且 > dx
    expect(interactions.hudState.type, SwipeAction.brightness);

    // 继续上滑 40px：ratio = 40 / (600*0.6) ≈ 0.11
    interactions.pointerMove(move(60, 260));
    expect(interactions.brightness, greaterThan(0.5), reason: '上滑应调高亮度');

    interactions.pointerUp(up(60, 300));
    await tester.pump(const Duration(seconds: 2)); // 冲掉 HUD 收起的 1000ms timer
  });

  testWidgets('左半屏水平拖动改为进度调节，不触发亮度', (tester) async {
    final interactions = await makeInteractions(tester);
    interactions.pointerDown(
      down(60, 300),
      swipeEnabled: true,
      width: viewWidth,
      height: viewHeight,
    );
    interactions.pointerMove(move(180, 302)); // dx=120，dy=2

    expect(
      interactions.hudState.type,
      SwipeAction.none,
      reason: '水平主方向不应进入亮度调节',
    );
    expect(interactions.feedback, contains('快进至'), reason: '左半屏横向拖动应给出进度预览');
    expect(
      interactions.chromeHintSuppressed,
      isTrue,
      reason: '拖动预览属进程态提示，不应唤出播放控件',
    );

    interactions.pointerUp(up(180, 302));
    expect(interactions.feedback, contains('快进至'), reason: '松手提交进度并给出最终提示');
    await tester.pump(const Duration(seconds: 2));
  });
}
