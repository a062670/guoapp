const allSourcesEnabled = bool.fromEnvironment('ALL_SOURCES');
const appName = allSourcesEnabled ? '真果鉴' : '红果鉴';
const appSlug = allSourcesEnabled ? 'zhenguojian' : 'hongguojian';
// BUILD_NUMBER 由构建脚本从 pubspec.yaml 的 version: x.y.z+N 自动注入
// （见 scripts/app_build.py build_number_from_pubspec），不要手动维护此文件
// 与 pubspec 两处；defaultValue 仅兜底绕过构建脚本直接 flutter build 的
// 场景，发新版本时若误触该路径，日志/关于页会显示这里的旧值。
const duanjuBuildNumber = int.fromEnvironment('BUILD_NUMBER', defaultValue: 138);
const appPackageId = 'com.duanju.duanju_app';
