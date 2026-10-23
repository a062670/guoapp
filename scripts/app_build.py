import base64
import re
from dataclasses import dataclass
from pathlib import Path


def build_number_from_pubspec():
    """从仓库根 pubspec.yaml 的 version: x.y.z+N 提取构建号 N。

    版本号的唯一权威源是 pubspec.yaml；lib/app_build.dart 的
    duanjuBuildNumber 只能通过 --dart-define=BUILD_NUMBER 编译期注入。
    此前没有任何构建入口传它，Dart 侧一直回落到 defaultValue(80)，
    导致启动日志与关于页的版本号永远停在上古值（例如已是 +138 的
    包仍显示 +80），排障时会按错误版本号误判代码新旧。此处统一从
    pubspec 提取，三个平台（android/ios/windows）的构建脚本都复用
    flutter_arguments，改这一处即全覆盖。提取不到（pubspec 没有 +N）
    时返回 None，由 Dart 侧 defaultValue 兜底。
    """
    pubspec = Path(__file__).resolve().parents[1] / 'pubspec.yaml'
    match = re.search(r'^version:\s*\S+\+(\d+)\s*$',
                      pubspec.read_text(encoding='utf-8'), re.MULTILINE)
    return match.group(1) if match else None


@dataclass(frozen=True)
class BuildVariant:
    all_sources: bool = False

    @property
    def name(self):
        return '真果鉴' if self.all_sources else '红果鉴'

    @property
    def slug(self):
        return 'zhenguojian' if self.all_sources else 'hongguojian'

    @property
    def arguments(self):
        return ['--all-sources'] if self.all_sources else []

    @property
    def flutter_arguments(self):
        arguments = ['--dart-define=ALL_SOURCES=' + str(self.all_sources).lower()]
        # BUILD_NUMBER 从 pubspec.yaml 的 +N 提取（详见 build_number_from_pubspec），
        # 保证启动日志/关于页版本号与实际代码版本一致。
        build_number = build_number_from_pubspec()
        if build_number:
            arguments.append('--dart-define=BUILD_NUMBER=' + build_number)
        return arguments

    @property
    def linker_flags(self):
        return '-s -w -X duanjuapp/native/core.buildAllSources=' + str(self.all_sources).lower()

    @classmethod
    def from_dart_defines(cls, encoded):
        values = {}
        for item in encoded.split(','):
            if not item:
                continue
            key, separator, value = base64.b64decode(item, validate=True).decode('utf-8').partition('=')
            if separator:
                values[key] = value
        return cls(values.get('ALL_SOURCES') == 'true')


def add_variant_argument(parser):
    parser.add_argument('--all-sources', action='store_true',
                        help='构建包含全部站源的真果鉴；默认构建仅红果的红果鉴')
