import 'dart:async';

import 'package:flutter/material.dart';

import 'core_bridge.dart';
import 'lan_screen.dart';
import 'local_store.dart';
import 'webdav_sync.dart';

class WebDavSyncScreen extends StatefulWidget {
  const WebDavSyncScreen({
    super.key,
    required this.store,
    required this.repository,
    this.syncOnOpen = false,
  });
  final LocalStore store;

  /// 片源仓库：同步片源订阅（对端导入的 LibreTV / drpy 源）与自动导入用。
  final AppRepository repository;

  /// 进入页面后立即同步一次。追剧页右上角入口用它，省掉"跳转后再点一次"。
  /// 注意与 WebDavSettings.autoSync（后台自动同步开关）区分：这里只是一次性
  /// 触发，不改变任何设置。
  final bool syncOnOpen;

  @override
  State<WebDavSyncScreen> createState() => _WebDavSyncScreenState();
}

class _WebDavSyncScreenState extends State<WebDavSyncScreen> {
  late WebDavSettings _saved = widget.store.webDavSettings;
  late WebDavSettings _form = _saved;
  late final TextEditingController _url = TextEditingController(
    text: _form.url,
  );
  late final TextEditingController _username = TextEditingController(
    text: _form.username,
  );
  late final TextEditingController _password = TextEditingController(
    text: _form.password,
  );
  late final TextEditingController _directory = TextEditingController(
    text: _form.directory,
  );
  bool _obscure = true;
  bool _busy = false;
  String _message = '';
  WebDavSyncOutcome? _outcome;

  /// 上次同步的非致命警告（个别片源导入失败等），逐条展示。
  List<String> _warnings = const [];

  @override
  void initState() {
    super.initState();
    if (!widget.syncOnOpen) return;
    if (widget.store.webDavSettings.configured) {
      unawaited(_sync());
    } else {
      // 没配置就不发无意义的请求，直接提示填配置。
      _message = '尚未配置 WebDAV，填写并保存后即可同步';
    }
  }

  bool get _dirty =>
      _form.enabled != _saved.enabled ||
      _form.url != _saved.url ||
      _form.username != _saved.username ||
      _form.password != _saved.password ||
      _form.directory != _saved.directory ||
      _form.autoSync != _saved.autoSync;

  WebDavSettings get _edited => WebDavSettings(
    enabled: _form.enabled,
    url: _url.text.trim(),
    username: _username.text.trim(),
    password: _password.text,
    directory: _directory.text.trim().isEmpty
        ? WebDavSettings.defaultDirectory
        : _directory.text.trim(),
    autoSync: _form.autoSync,
    lastSyncAt: _saved.lastSyncAt,
  );

  @override
  void dispose() {
    _url.dispose();
    _username.dispose();
    _password.dispose();
    _directory.dispose();
    super.dispose();
  }

  Future<void> _save() async {
    final next = _edited;
    if (next.directoryUri == null) {
      setState(() => _message = '同步地址无效：需要 http/https 地址，目录名仅限字母、数字、- 和 _');
      return;
    }
    setState(() {
      _busy = true;
      _message = '';
    });
    try {
      await widget.store.saveWebDavSettings(
        next.copyWith(clearLastError: true),
      );
      if (!mounted) return;
      setState(() {
        _saved = next.copyWith(clearLastError: true);
        _form = _saved;
        _message = '已保存';
      });
    } catch (failure) {
      if (mounted) setState(() => _message = failure.toString());
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _test() async {
    final next = _edited;
    final directory = next.directoryUri;
    if (directory == null) {
      setState(() => _message = '同步地址无效：需要 http/https 地址，目录名仅限字母、数字、- 和 _');
      return;
    }
    setState(() {
      _busy = true;
      _message = '';
    });
    final transport = HttpWebDavTransport(
      username: next.username,
      password: next.password,
    );
    try {
      await transport.ensureDirectory(directory);
      final files = await transport.list(directory);
      if (!mounted) return;
      setState(
        () => _message = '连接成功，同步目录内有 ${files.length} 个设备文件',
      );
    } catch (failure) {
      if (mounted) setState(() => _message = failure.toString());
    } finally {
      transport.close();
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _sync() async {
    if (_dirty) await _save();
    if (!mounted) return;
    setState(() {
      _busy = true;
      _message = '';
      _outcome = null;
      _warnings = const [];
    });
    final settings = widget.store.webDavSettings;
    final transport = HttpWebDavTransport(
      username: settings.username,
      password: settings.password,
    );
    try {
      final outcome = await WebDavSynchronizer(
        store: widget.store,
        transport: transport,
        repository: widget.repository,
      ).synchronize();
      if (!mounted) return;
      setState(() {
        _outcome = outcome;
        _saved = widget.store.webDavSettings;
        _form = _saved;
        final summary = outcome.hasChanges ? '同步完成' : '已是最新，没有变化';
        final extra = <String>[
          if (outcome.sourcePrefs > 0) '片源偏好 ${outcome.sourcePrefs} 项',
          if (outcome.sourceImports > 0) '导入片源 ${outcome.sourceImports} 个',
        ];
        _message = extra.isEmpty
            ? summary
            : '$summary（${extra.join('，')}）';
        _warnings = outcome.warnings;
      });
    } catch (failure) {
      if (mounted) setState(() => _message = failure.toString());
    } finally {
      transport.close();
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) => AnimatedBuilder(
    animation: widget.store,
    builder: (context, _) {
      final conflicts = widget.store.locked
          ? 0
          : widget.store.lanDocument.records.values
                .where(
                  (record) =>
                      record.conflicts > 0 &&
                      widget.store.allowsSource(record.drama.source),
                )
                .length;
      return Scaffold(
        appBar: AppBar(title: const Text('WebDAV 同步')),
        body: Center(
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 720),
            child: ListView(
              padding: const EdgeInsets.all(16),
              children: [
                Card(
                  child: Padding(
                    padding: const EdgeInsets.all(16),
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(
                          '通过 WebDAV 网盘在手机、电脑、电视之间同步追剧、观看状态和续播进度，与「设备互联」范围相同。',
                          style: Theme.of(context).textTheme.bodyMedium,
                        ),
                        const Padding(
                          padding: EdgeInsets.only(top: 8),
                          child: Text(
                            '每台设备上传自己的记录文件，同步时下载其他设备的记录并按版本合并，'
                            '同一处修改不一致时保留双方版本，可在「设备互联」页逐项选择。'
                            '账号密码只保存在本机，不明文显示，不进入导出备份。',
                          ),
                        ),
                        const Padding(
                          padding: EdgeInsets.only(top: 8),
                          child: Text(
                            '同步范围还包括片源：已导入的 LibreTV / drpy 片源会出现在其他设备上'
                            '（自动导入），收藏 / 隐藏状态按最新操作时间合并。'
                            '注意：粘贴导入的 drpy 脚本源没有可同步的地址，不会跨设备传输。',
                          ),
                        ),
                      ],
                    ),
                  ),
                ),
                SwitchListTile(
                  key: const ValueKey('webdav-enabled'),
                  value: _form.enabled,
                  title: const Text('启用 WebDAV 同步'),
                  onChanged: _busy
                      ? null
                      : (value) => setState(() => _form = _form.copyWith(enabled: value)),
                ),
                SwitchListTile(
                  value: _form.autoSync,
                  title: const Text('打开应用时自动同步'),
                  subtitle: const Text('启动后自动同步一次，失败不影响使用'),
                  onChanged: _busy
                      ? null
                      : (value) => setState(() => _form = _form.copyWith(autoSync: value)),
                ),
                TextField(
                  controller: _url,
                  enabled: !_busy,
                  keyboardType: TextInputType.url,
                  decoration: const InputDecoration(
                    labelText: '服务器地址',
                    hintText: 'https://dav.jianguoyun.com/dav/',
                    helperText:
                        '填到服务提供的 WebDAV 目录为止，坚果云要在域名后带上 /dav/，Nextcloud 通常形如 /remote.php/dav/files/用户名/',
                  ),
                ),
                const SizedBox(height: 12),
                TextField(
                  controller: _username,
                  enabled: !_busy,
                  decoration: const InputDecoration(
                    labelText: '账号',
                    isDense: true,
                  ),
                ),
                const SizedBox(height: 12),
                TextField(
                  controller: _password,
                  enabled: !_busy,
                  obscureText: _obscure,
                  decoration: InputDecoration(
                    labelText: '密码',
                    isDense: true,
                    suffixIcon: IconButton(
                      icon: Icon(
                        _obscure
                            ? Icons.visibility_outlined
                            : Icons.visibility_off_outlined,
                      ),
                      tooltip: _obscure ? '显示密码' : '隐藏密码',
                      onPressed: () => setState(() => _obscure = !_obscure),
                    ),
                  ),
                ),
                const SizedBox(height: 12),
                TextField(
                  controller: _directory,
                  enabled: !_busy,
                  decoration: const InputDecoration(
                    labelText: '同步目录',
                    hintText: WebDavSettings.defaultDirectory,
                    helperText: '服务器上用于存放各设备记录文件的目录',
                  ),
                ),
                const SizedBox(height: 16),
                Wrap(
                  spacing: 12,
                  runSpacing: 12,
                  children: [
                    FilledButton.icon(
                      key: const ValueKey('webdav-save'),
                      onPressed: _busy || !_dirty ? null : _save,
                      icon: const Icon(Icons.save_outlined),
                      label: const Text('保存设置'),
                    ),
                    OutlinedButton.icon(
                      key: const ValueKey('webdav-test'),
                      onPressed: _busy || !_form.configured ? null : _test,
                      icon: const Icon(Icons.wifi_tethering_rounded),
                      label: const Text('测试连接'),
                    ),
                    FilledButton.icon(
                      key: const ValueKey('webdav-sync'),
                      onPressed: _busy || !_form.configured ? null : _sync,
                      icon: const Icon(Icons.cloud_sync_outlined),
                      label: const Text('立即同步'),
                    ),
                  ],
                ),
                if (_saved.lastSyncAt != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 12),
                    child: Text('上次同步：${_saved.lastSyncAt!.toLocal()}'),
                  ),
                if (_saved.lastError != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 4),
                    child: SelectableText(
                      _saved.lastError!,
                      style: TextStyle(
                        color: Theme.of(context).colorScheme.error,
                      ),
                    ),
                  ),
                if (_outcome != null)
                  Padding(
                    padding: const EdgeInsets.only(top: 12),
                    child: Text(_outcome!.label),
                  ),
                if (_message.isNotEmpty)
                  Padding(
                    padding: const EdgeInsets.only(top: 12),
                    child: SelectableText(_message),
                  ),
                for (final warning in _warnings)
                  Padding(
                    padding: const EdgeInsets.only(top: 4),
                    child: SelectableText(
                      warning,
                      style: TextStyle(
                        color: Theme.of(context).colorScheme.error,
                      ),
                    ),
                  ),
                if (_busy) const Padding(
                  padding: EdgeInsets.only(top: 12),
                  child: LinearProgressIndicator(),
                ),
                // 追剧页右上角的同步按钮已改为直达本页，这里补一个反向入口，
                // 免得局域网同步只能从「设置」里绕进去。
                Card(
                  key: const ValueKey('webdav-open-lan'),
                  child: ListTile(
                    leading: const Icon(Icons.devices_rounded),
                    title: const Text('设备互联'),
                    subtitle: const Text('局域网自动同步追剧与推送播放'),
                    trailing: const Icon(Icons.chevron_right_rounded),
                    onTap: () => openLanSync(context),
                  ),
                ),
                if (conflicts > 0)
                  Card(
                    key: const ValueKey('webdav-conflicts'),
                    child: ListTile(
                      leading: const Icon(Icons.merge_rounded),
                      title: Text('$conflicts 项记录待处理'),
                      subtitle: const Text('合并时保留了多个版本，去选择要保留的记录'),
                      trailing: const Icon(Icons.chevron_right_rounded),
                      onTap: () => openLanSync(context),
                    ),
                  ),
              ],
            ),
          ),
        ),
      );
    },
  );
}
