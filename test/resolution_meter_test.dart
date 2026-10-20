import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:duanju_app/resolution_meter.dart';

// 清晰度自动检测进度条：按检测到的片源高度折算档位并填充进度。
// 节点固定 480P / 1080P / 2K / 4K，用对数刻度定位，1080P 大致落在条中部。

void main() {
  test('档位折算：按高度取不超过该高度的最大节点', () {
    expect(resolutionLevelOf(0), isNull);
    expect(resolutionLevelOf(-10), isNull);
    expect(resolutionLevelOf(240)?.label, isNull); // 低于最低节点不猜档
    expect(resolutionLevelOf(480)?.label, '480P');
    expect(resolutionLevelOf(720)?.label, '480P');
    expect(resolutionLevelOf(1080)?.label, '1080P');
    expect(resolutionLevelOf(1440)?.label, '2K');
    expect(resolutionLevelOf(2160)?.label, '4K');
    // 高于 4K 归入 4K 档。
    expect(resolutionLevelOf(4320)?.label, '4K');
  });

  test('填充进度：对数刻度、单调、夹在 0..1', () {
    expect(resolutionProgressOf(0), 0);
    expect(resolutionProgressOf(360), 0);
    expect(resolutionProgressOf(2160), 1);
    expect(resolutionProgressOf(4320), 1); // 超过 4K 不溢出
    // 1080P 应该落在条中部（对数刻度 ~0.55），而不是贴着左端。
    final p1080 = resolutionProgressOf(1080);
    expect(p1080, greaterThan(0.4));
    expect(p1080, lessThan(0.7));
    // 单调性：更高的分辨率填充更多。
    expect(
      resolutionProgressOf(720),
      lessThan(resolutionProgressOf(1080)),
    );
    expect(
      resolutionProgressOf(1080),
      lessThan(resolutionProgressOf(2160)),
    );
    // 节点定位也单调。
    for (var i = 1; i < resolutionLevels.length; i++) {
      expect(
        resolutionLevels[i].position,
        greaterThan(resolutionLevels[i - 1].position),
      );
    }
  });

  testWidgets('未检测到尺寸时显示待检测文案，进度为 0', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: ResolutionMeter(width: 0, height: 0, label: '清晰度'),
        ),
      ),
    );
    expect(find.text('正在检测片源尺寸，起播后显示实际分辨率'), findsOneWidget);
    expect(find.text('清晰度'), findsOneWidget);
    // 4 个节点圆点都在。
    for (final node in resolutionLevels) {
      expect(find.byKey(resolutionNodeKey(node.label)), findsOneWidget);
    }
  });

  testWidgets('检测到 1080P 时节点高亮并显示像素说明', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: ResolutionMeter(width: 1920, height: 1080),
        ),
      ),
    );
    expect(find.text('已检测 1080P (1920×1080)'), findsOneWidget);
    expect(find.text('480P'), findsOneWidget);
    expect(find.text('1080P'), findsOneWidget);
    expect(find.text('2K'), findsOneWidget);
    expect(find.text('4K'), findsOneWidget);
  });

  testWidgets('showCaption=false 时不显示说明行但保留节点', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: ResolutionMeter(
            width: 3840,
            height: 2160,
            compact: true,
            showCaption: false,
          ),
        ),
      ),
    );
    expect(find.textContaining('已检测'), findsNothing);
    expect(find.byKey(resolutionNodeKey('4K')), findsOneWidget);
    expect(find.text('4K'), findsOneWidget);
  });
}
