import 'dart:async';
import 'dart:io';

import 'package:flutter/foundation.dart';
import 'package:flutter_inappwebview/flutter_inappwebview.dart';

/// WebView 嗅探（TVBox 解析类型 0 / 4 的等价实现）。
///
/// 背景：这类解析规则不提供接口，而是要求用一个真实浏览器打开播放页，
/// 页面里的播放器自己发出 m3u8 / mp4 请求，客户端拦截这个请求即得到直链。
/// Go 侧没有浏览器环境，因此由 Dart 侧用无界面的 Headless WebView 完成：
///
///  1. 加载解析页（移动端 UA、关闭「必须先点播放」的限制）；
///  2. 拦截网络请求与已加载资源，命中媒体后缀即完成；
///  3. 页面已渲染但还没发请求时，用 JS 兜底读取 `<video>/<source>` 的 src；
///  4. 超时 / 平台不支持 / 插件异常一律返回 null，由调用方降级提示。
///
/// 仅 Android / iOS 可用（插件未提供 Windows 实现），其他平台调用方需
/// 先检查 [supported]。
class WebViewSniffer {
  WebViewSniffer._();

  static final WebViewSniffer shared = WebViewSniffer._();

  /// 媒体地址判定：常见容器后缀 + HLS 清单。查询串/锚点不影响匹配。
  static final RegExp _mediaPattern = RegExp(
    r'\.(m3u8|mp4|flv|mkv|mov|ts)(?![a-z0-9])(\?|#|$)',
    caseSensitive: false,
  );

  static bool looksLikeMediaUrl(String url) {
    final text = url.trim();
    if (text.isEmpty || !text.startsWith('http')) return false;
    return _mediaPattern.hasMatch(text);
  }

  /// 当前平台是否具备 WebView 嗅探能力。
  static bool get supported {
    if (kIsWeb) return false;
    try {
      return Platform.isAndroid || Platform.isIOS;
    } catch (_) {
      // 无法判定平台的宿主（测试环境）按不支持处理。
      return false;
    }
  }

  static const _mobileUserAgent =
      'Mozilla/5.0 (Linux; Android 12; Pixel 6) AppleWebKit/537.36 '
      '(KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36';

  /// 嗅探播放地址；找不到返回 null。
  Future<String?> sniff(
    String pageUrl, {
    String referer = '',
    Duration timeout = const Duration(seconds: 25),
  }) async {
    if (!supported || pageUrl.trim().isEmpty) return null;
    final completer = Completer<String?>();
    HeadlessInAppWebView? webView;
    Timer? timer;

    void settle(String? value) {
      if (completer.isCompleted) return;
      completer.complete(
        value != null && looksLikeMediaUrl(value) ? value.trim() : null,
      );
    }

    void consider(String? url) {
      if (url == null) return;
      if (looksLikeMediaUrl(url)) settle(url);
    }

    try {
      webView = HeadlessInAppWebView(
        initialUrlRequest: URLRequest(
          url: WebUri(pageUrl.trim()),
          headers: referer.trim().isEmpty ? null : {'Referer': referer.trim()},
        ),
        initialSettings: InAppWebViewSettings(
          javaScriptEnabled: true,
          mediaPlaybackRequiresUserGesture: false,
          userAgent: _mobileUserAgent,
          // Android：开启后才会回调 shouldInterceptRequest。
          useShouldInterceptRequest: true,
          // 混合内容（https 页面加载 http 媒体）在嗅探场景很常见。
          mixedContentMode: MixedContentMode.MIXED_CONTENT_ALWAYS_ALLOW,
          transparentBackground: true,
        ),
        shouldInterceptRequest: (controller, request) async {
          consider(request.url.toString());
          return null; // 不阻断，页面照常加载
        },
        onLoadResource: (controller, resource) {
          consider(resource.url.toString());
        },
        onLoadStop: (controller, url) async {
          // JS 兜底：播放器已把地址写进 video/source 但尚未发请求。
          try {
            final found = await controller.evaluateJavascript(
              source: '''
                (function(){
                  var v = document.querySelector('video');
                  if (v) {
                    if (v.currentSrc) return v.currentSrc;
                    if (v.src) return v.src;
                    var s = v.querySelector('source');
                    if (s && s.src) return s.src;
                  }
                  var list = document.querySelectorAll('source');
                  for (var i = 0; i < list.length; i++) {
                    if (list[i].src) return list[i].src;
                  }
                  return '';
                })();
              ''',
            );
            if (found is String) consider(found);
          } catch (_) {
            // 页面禁用了 JS 或已销毁：忽略，等拦截或超时。
          }
        },
        onReceivedError: (controller, request, error) {
          // 主文档加载失败时尽早结束，避免白等超时。
          if (request.isForMainFrame == true) settle(null);
        },
      );
      await webView.run();
      timer = Timer(timeout, () => settle(null));
      return await completer.future;
    } catch (_) {
      // 插件缺失 / 平台不支持 / WebView 初始化失败：交给调用方降级。
      return null;
    } finally {
      timer?.cancel();
      try {
        await webView?.dispose();
      } catch (_) {
        // 已销毁或插件不可用时忽略。
      }
    }
  }
}
