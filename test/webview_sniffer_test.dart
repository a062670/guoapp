import 'package:flutter_test/flutter_test.dart';

import 'package:duanju_app/webview_sniffer.dart';

// type 0 / 4 的 WebView 嗅探无法在测试环境真正启动浏览器（Linux 无实现），
// 这里覆盖地址判定的纯逻辑：它决定了「拦截到哪个请求算找到播放地址」。
void main() {
  test('媒体地址判定覆盖常见容器与查询串', () {
    expect(
      WebViewSniffer.looksLikeMediaUrl('https://cdn.test/a.m3u8?token=1'),
      isTrue,
    );
    expect(
      WebViewSniffer.looksLikeMediaUrl('https://cdn.test/a.mp4#t=10'),
      isTrue,
    );
    expect(WebViewSniffer.looksLikeMediaUrl('https://cdn.test/a.flv'), isTrue);
    expect(WebViewSniffer.looksLikeMediaUrl('https://cdn.test/A.M3U8'), isTrue);
    expect(
      WebViewSniffer.looksLikeMediaUrl('https://cdn.test/video/index.ts?x=1'),
      isTrue,
    );
  });

  test('非媒体请求与非法地址不误判', () {
    expect(
      WebViewSniffer.looksLikeMediaUrl('https://cdn.test/page.html'),
      isFalse,
    );
    expect(
      WebViewSniffer.looksLikeMediaUrl('https://cdn.test/pic.jpg'),
      isFalse,
    );
    expect(
      WebViewSniffer.looksLikeMediaUrl('https://cdn.test/app.js'),
      isFalse,
    );
    expect(WebViewSniffer.looksLikeMediaUrl('/relative/a.m3u8'), isFalse);
    expect(WebViewSniffer.looksLikeMediaUrl(''), isFalse);
    // 后缀必须完整匹配（m3u8x 不是播放列表）。
    expect(
      WebViewSniffer.looksLikeMediaUrl('https://cdn.test/file.m3u8x'),
      isFalse,
    );
  });
}
