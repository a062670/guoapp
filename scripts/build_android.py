import argparse
import os
import platform
import shutil
import subprocess
import sys
from pathlib import Path

from build_mirrors import china_mirror_environment, mirrored_pub_lockfile
from app_build import BuildVariant, add_variant_argument

root = Path(__file__).resolve().parents[1]


def patch_android_plugins(env):
    """修补第三方插件里与 AGP 9 不兼容的 Gradle 写法。

    flutter_inappwebview_android 1.1.3 的 build.gradle 仍使用
    getDefaultProguardFile('proguard-android.txt')，AGP 9 起该文件因包含
    -dontoptimize 被移除，会在评估插件工程时直接失败：

        `getDefaultProguardFile('proguard-android.txt')` is no longer supported …

    这是插件侧的已知问题（其 1.2.0-beta 已改为 -optimize 版本）。升级到
    稳定版前，在 pub 缓存里做一次幂等替换：只改这一行，不做其他改动。
    找不到插件目录（未安装 / 版本已修复）时静默跳过，不影响构建。
    """
    cache = Path(env.get('PUB_CACHE') or Path.home() / '.pub-cache')
    pattern = 'hosted/*/flutter_inappwebview_android-*/android/build.gradle'
    patched = 0
    for gradle in cache.glob(pattern):
        try:
            text = gradle.read_text(encoding='utf-8')
        except OSError:
            continue
        if "getDefaultProguardFile('proguard-android.txt')" not in text:
            continue
        gradle.write_text(
            text.replace(
                "getDefaultProguardFile('proguard-android.txt')",
                "getDefaultProguardFile('proguard-android-optimize.txt')",
            ),
            encoding='utf-8',
        )
        patched += 1
    if patched:
        print(f'已修补 {patched} 个 flutter_inappwebview_android 的 AGP 9 兼容问题。', flush=True)


parser = argparse.ArgumentParser()
parser.add_argument('--abi', action='append', choices=['arm64-v8a', 'armeabi-v7a', 'x86_64'])
parser.add_argument('--cn-mirrors', action='store_true', help='使用 Flutter 中国镜像和阿里云 Maven 镜像')
add_variant_argument(parser)
options = parser.parse_args()
variant = BuildVariant(options.all_sources)
environment = os.environ.copy()
if platform.system() == 'Darwin':
    environment['LANG'] = 'en_US.UTF-8'
    environment['LC_ALL'] = 'en_US.UTF-8'
environment.setdefault('GOPROXY', 'https://goproxy.cn,direct')
environment.setdefault('GOSUMDB', 'off')
flutter = shutil.which('flutter')
if not flutter:
    raise SystemExit('请先将 Flutter SDK 的 bin 目录加入 PATH。')
abi_args = [item for abi in options.abi or [] for item in ['--abi', abi]]
with china_mirror_environment(environment, options.cn_mirrors) as env, mirrored_pub_lockfile(root, env):
    if options.cn_mirrors:
        print('本次构建启用国内依赖镜像，保留官方 Maven 仓库备用。', flush=True)
    subprocess.run([sys.executable, str(root / 'scripts' / 'build_native.py'), '--platform', 'android', *abi_args, *variant.arguments],
                   cwd=root, env=env, check=True)
    subprocess.run([flutter, 'pub', 'get'], cwd=root, env=env, check=True)
    # pub get 之后插件才落到缓存里，构建前做一次兼容性修补。
    patch_android_plugins(env)
    build_args = [flutter, 'build', 'apk', '--release', '--split-per-abi', '--no-pub', *variant.flutter_arguments]
    if options.abi:
        targets = {'arm64-v8a': 'android-arm64', 'armeabi-v7a': 'android-arm', 'x86_64': 'android-x64'}
        build_args += ['--target-platform', ','.join(targets[abi] for abi in options.abi)]
    subprocess.run(build_args, cwd=root, env=env, check=True)
    subprocess.run([sys.executable, str(root / 'scripts' / 'package_release.py'), '--platform', 'android', *abi_args, *variant.arguments],
                   cwd=root, env=env, check=True)
