import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

export 'app_build.dart';

const appVersion = '0.2.74';

ThemeData televisionTheme(ThemeData theme) {
  final colors = theme.colorScheme;
  final focusSide = WidgetStateProperty.resolveWith<BorderSide?>(
    (states) => states.contains(WidgetState.focused)
        ? BorderSide(color: colors.primary, width: 3)
        : null,
  );
  final focusBackground = WidgetStateProperty.resolveWith<Color?>(
    (states) =>
        states.contains(WidgetState.focused) ? colors.primaryContainer : null,
  );
  final button = ButtonStyle(
    side: focusSide,
    minimumSize: const WidgetStatePropertyAll(Size(52, 48)),
    textStyle: const WidgetStatePropertyAll(TextStyle(fontSize: 17)),
  );
  return theme.copyWith(
    focusColor: colors.primaryContainer,
    iconButtonTheme: IconButtonThemeData(
      style: button.copyWith(backgroundColor: focusBackground),
    ),
    filledButtonTheme: FilledButtonThemeData(style: button),
    outlinedButtonTheme: OutlinedButtonThemeData(style: button),
    textButtonTheme: TextButtonThemeData(
      style: button.copyWith(backgroundColor: focusBackground),
    ),
    inputDecorationTheme: theme.inputDecorationTheme.copyWith(
      focusedBorder: OutlineInputBorder(
        borderRadius: BorderRadius.circular(16),
        borderSide: BorderSide(color: colors.primary, width: 3),
      ),
    ),
  );
}

class AppDevice {
  const AppDevice({this.television = false, this.version = appVersion});
  final bool television;
  final String version;
  static const channel = MethodChannel('duanju/device');

  /// 最近一次 [detect] 的结果（失败时即当次的 fallback）。
  ///
  /// main() 在 runApp 前 await 过一次 [detect]，因此任何界面构建时该值必然
  /// 已就绪。供拿不到 BuildContext 的场景（如 PlayerScreen.initState）同步
  /// 读取电视端判定——AppLayout.isTelevision 依赖 InheritedWidget，在
  /// initState 里不可用。
  static AppDevice? cached;

  static Future<AppDevice> detect({
    AppDevice fallback = const AppDevice(),
  }) async {
    if (defaultTargetPlatform != TargetPlatform.android) {
      cached = const AppDevice();
      return const AppDevice();
    }
    try {
      final data = await channel
          .invokeMapMethod<String, dynamic>('deviceInfo')
          .timeout(const Duration(seconds: 2));
      final television = data?['television'];
      if (television is! bool) {
        cached = fallback;
        return fallback;
      }
      final version = data?['version'];
      final detected = AppDevice(
        television: television,
        version: version is String && version.isNotEmpty
            ? version
            : fallback.version,
      );
      cached = detected;
      return detected;
    } on PlatformException {
      cached = fallback;
      return fallback;
    } on MissingPluginException {
      cached = fallback;
      return fallback;
    } on TimeoutException {
      cached = fallback;
      return fallback;
    }
  }

  static Future<double> getBrightness() async {
    if (defaultTargetPlatform != TargetPlatform.android) return 0.5;
    try {
      final value = await channel.invokeMethod<double>('getBrightness');
      return (value ?? 0.5).clamp(0.0, 1.0);
    } catch (_) {
      return 0.5;
    }
  }

  static Future<void> setBrightness(double brightness) async {
    if (defaultTargetPlatform != TargetPlatform.android) return;
    try {
      await channel.invokeMethod('setBrightness', {'brightness': brightness.clamp(0.01, 1.0)});
    } catch (_) {}
  }

  static Future<void> resetBrightness() async {
    if (defaultTargetPlatform != TargetPlatform.android) return;
    try {
      await channel.invokeMethod('resetBrightness');
    } catch (_) {}
  }
}

class AppLayout extends InheritedWidget {
  const AppLayout({
    super.key,
    required this.television,
    this.version = appVersion,
    required super.child,
  });
  final bool television;
  final String version;

  static bool isTelevision(BuildContext context) =>
      context.dependOnInheritedWidgetOfExactType<AppLayout>()?.television ??
      false;
  static String versionOf(BuildContext context) =>
      context.dependOnInheritedWidgetOfExactType<AppLayout>()?.version ??
      appVersion;

  @override
  bool updateShouldNotify(AppLayout oldWidget) =>
      television != oldWidget.television || version != oldWidget.version;
}
