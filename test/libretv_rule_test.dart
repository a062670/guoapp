import 'package:duanju_app/libretv.dart';
import 'package:flutter_test/flutter_test.dart';

// 规则型 Spider 片源（XBPQ / 海阔 / Bili）的模型行为。
void main() {
  group('LibreTVSite rule engine', () {
    test('规则源使用引擎标签与说明', () {
      const xbpq = LibreTVSite(
        key: 'wwgz',
        name: '旺旺影视',
        api: 'https://vip.example.com',
        engine: 'xbpq',
        ruleUrl: 'https://rules.example.com/wwgz.json',
      );
      expect(xbpq.isRule, isTrue);
      expect(xbpq.typeLabel, 'XBPQ 规则');
      expect(xbpq.engineHint, contains('无需 jar'));
      expect(xbpq.toSourceSite().description, contains('XBPQ 规则'));

      const hiker = LibreTVSite(key: 'a', name: 'A', api: 'https://a.com', engine: 'hiker');
      expect(hiker.typeLabel, '海阔规则');
      const bili = LibreTVSite(key: 'b', name: 'B', api: 'https://b.com', engine: 'bili');
      expect(bili.typeLabel, 'Bili');
    });

    test('采集站保持原有标签', () {
      const json = LibreTVSite(key: 'a', name: 'A', api: 'https://a.com/provide/vod');
      expect(json.isRule, isFalse);
      expect(json.typeLabel, 'JSON 采集');
      expect(json.ruleSubscriptionJson, isEmpty);
      final xml = LibreTVSite(
        key: 'b',
        name: 'B',
        api: 'https://b.com/at/xml/',
        type: 2,
        engine: '',
      );
      expect(xml.typeLabel, 'XML 采集');
    });

    test('规则源可重建为单站点订阅（跨设备同步用）', () {
      const site = LibreTVSite(
        key: 'wwgz',
        name: '旺旺"影视"',
        api: 'https://vip.example.com',
        engine: 'xbpq',
        ruleUrl: 'https://rules.example.com/wwgz.json',
      );
      final json = site.ruleSubscriptionJson;
      expect(json, contains('"api":"csp_XBPQ"'));
      expect(json, contains('"key":"wwgz"'));
      expect(json, contains('"ext":"https://rules.example.com/wwgz.json"'));
      // 名称里的引号必须被转义，否则订阅 JSON 会被破坏。
      expect(json, contains(r'\"影视\"'));
    });

    test('内联规则源没有规则地址时不参与同步', () {
      const inline = LibreTVSite(
        key: 'inline',
        name: '内联规则',
        api: 'https://a.com',
        engine: 'hiker',
      );
      expect(inline.ruleSubscriptionJson, isEmpty);
    });

    test('fromJson 读取 engine / ruleUrl', () {
      final site = LibreTVSite.fromJson(const {
        'key': 'k',
        'name': 'N',
        'api': 'https://a.com',
        'type': 1,
        'engine': 'bili',
        'ruleUrl': 'https://rules.example.com/bili.json',
      });
      expect(site.engine, 'bili');
      expect(site.ruleUrl, 'https://rules.example.com/bili.json');
      expect(site.spiderApi, 'csp_Bili');
    });
  });
}
