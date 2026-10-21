# 项目长期记忆（OctPlugins）

## 目录与发布位

- 仓库根：`D:\Project\ElectronProject\OctPlugins`（开发态：`ui/` 宿主 + `plugins/` + `kernel/`）。
- **发布位：`D:\pyex\OctPlugins`**（Electron 便携目录，无 asar）。布局：
  `resources/app/{index.html,browser-panel.js,build/,vendor/,node_modules/ws}`、`resources/plugins/`、
  `resources/state/`、`resources/kernel|runtime|bin|sdk|config|resources`。
- 打包：`.\package.ps1 -OutDir D:\pyex\OctPlugins`；`-SkipUI` 复用已编译的 `ui/build`；
  `-ResourcesSrc <已打包 resources>` 复用 runtime 避免重建 venv（离线）。
  脚本在 `OctPluginsHost.exe` 运行时直接 Die，必须先退出（托盘 → 退出），kerneld.exe 是它的子进程。

## 打包脚本的坑

- `package.ps1` 复制 `ui/index.html`、`ui/browser-panel.js`、`ui/vendor`、`ui/build`（Step 4/8），
  以及 `plugins/*`（Step 5/8）；state 只 seed `perms.json` / `overrides.json` / `plugins/proxy`。
- **`package.ps1` 不复制 `state/registry.json`** → 打包目录里的 manifestHash 基线是上次运行时留下的。
  改了 `plugins/<id>/manifest.json` 就必须让该基线跟上（改仓库的 `state/registry.json`，
  或删掉打包目录 registry 里对应条目让首次启动重新钉），否则 tamperCheck 拒绝启动插件
  （`kernel/internal/lifecycle/manager.go:670`，sha256 原始字节比对）。

## 内嵌浏览器（browser 插件）

- 宿主实现：`ui/browser-panel.js` + `ui/index.html#panel-plugin.browser`；插件 `plugins/browser/` 只管
  打开意图与 startup 清单，不碰 Electron。
- 标签生命周期：lazy / live / sleep(discard) / crashed，阈值在 `CFG`（maxTabs 12、idleDiscardMs 15min、
  panelColdMs 10min、taskCloseMs 60s）。
- 未迁移 WebContentsView（决定记录在 2026-10-21.md）。
