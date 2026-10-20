import 'package:flutter_test/flutter_test.dart';

import 'package:duanju_app/models.dart';

// vod_time / XML last：源站更新时间，随 Drama 在 Go→Dart→备份间透传。
void main() {
  test('Drama 透传 updateTime（json 往返与旧数据兼容）', () {
    final drama = Drama.fromJson(<String, dynamic>{
      'id': 'libretv:dsd:101',
      'source': 'libretv:dsd',
      'title': '测试剧',
      'updateTime': '2026-10-19 20:15',
      'tags': ['剧情', '内地'],
    });
    expect(drama.updateTime, '2026-10-19 20:15');
    expect(Drama.fromJson(drama.toJson()).updateTime, '2026-10-19 20:15');

    // 旧备份 / 旧核心没有 updateTime：缺省空串，不抛错。
    final legacy = Drama.fromJson(<String, dynamic>{
      'id': 'hongguo:1',
      'source': 'hongguo',
      'title': '旧数据',
    });
    expect(legacy.updateTime, '');
  });

  test('merge 时新数据为空保留旧更新时间', () {
    final previous = Drama.fromJson(<String, dynamic>{
      'id': 'libretv:dsd:101',
      'source': 'libretv:dsd',
      'title': '测试剧',
      'updateTime': '2026-10-18 10:00',
    });
    final empty = Drama.fromJson(<String, dynamic>{
      'id': 'libretv:dsd:101',
      'source': 'libretv:dsd',
      'title': '测试剧',
    });
    expect(empty.merge(previous).updateTime, '2026-10-18 10:00');

    final fresh = Drama.fromJson(<String, dynamic>{
      'id': 'libretv:dsd:101',
      'source': 'libretv:dsd',
      'title': '测试剧',
      'updateTime': '2026-10-19 20:15',
    });
    expect(previous.merge(fresh).updateTime, '2026-10-19 20:15');
  });
}
