# OctPlugins 宿主搭建方案

## Context（为什么做这件事）

`d:\Project\ElectronProject\OctPlugins` 目前只包含《理想架构.md》规范和一套共享 `resources/`（icons、logo、theme/plugin.css、shanhe12），**软件本身（宿主 UI + 内核 + 插件）尚未存在**。

用户要求：依据《理想架构.md》的架构规范，参照 `D:\Project\ElectronProject\OCTplugin` 这个已可运行的参考实现的 UI，做出一个新的 **OctPlugins** 软件。

参考实现 OCTplugin 中可复用的关键资产：
- 宿主 UI：`host/index.html`（约 100KB 墨韵工作台界面，含顶栏/侧栏/设置子窗口/命令面板/插件 iframe 装配/权限/热键等）与 `host/main.js`（主进程：拉起 Go 内核、stdout 读 auth、WS 连接、rpc 桥、窗口控制、托盘、热键、资源设置）。
- Go 内核：`kernel/`（`cmd/kerneld/main.go` 输出 `{port,token}` auth 行并起 WS；`internal/ipc/ws.go` 方法分发表；`internal/supervisor` 进程管理；`internal/perms`；`internal/protocol`）。
- 参考插件（web UI）：`home`、`md`、`demo_web` 等，供侧栏/iframe 演示。

## 目标交付

在 `OctPlugins` 下构建一个**可运行**的宿主骨架，遵循《理想架构.md》的分层（Electron 纯UI + Go 内核 + WS 通信），UI 视觉与交互对齐参考实现，内置 1–2 个 web 插件用于演示侧栏/iframe 装配。

## 目录结构（对齐《理想架构.md》§4）

```
OctPlugins/
├── 理想架构.md
├── resources/                    # 已有：icons / logo / theme / shanhe12
├── ui/                           # Electron 宿主（端口/索引从 host/ 迁入并改名 ui）
│   ├── index.html
│   ├── main.js
│   ├── package.json              # electron + ws
│   └── .npmrc
├── kernel/                       # Go 内核（从 OCTplugin/kernel 迁入，更名为 kernel/…）
│   ├── cmd/kerneld/main.go
│   ├── internal/{ipc,supervisor,perms,protocol}
│   ├── go.mod / go.sum
│   └── (构建产物 kerneld.exe)
├── plugins/                      # 示例 web 插件
│   ├── home/ resources/manifest.json + ui/
│   └── demo_web/ …
├── config/                       # 宿主可写（user-settings.json 占位）
├── state/                        # 宿主独占写（registry.json/secrets.json 占位）
├── build.ps1                     # 一键：go build kernel + npm install ui
└── README.md（可选，仅当用户要求，默认不建）
```

> 说明：参考实现用 `host/`、路径 `RUNTIME_ROOT=__dirname/..`；为对齐架构规范改为 `ui/`，主进程路径常量需相应调整（见下）。

## 实施步骤

### 1. 迁入 Go 内核并适配
- 复制 `OCTplugin/kernel/cmd`、`internal`、`go.mod`、`go.sum` 到 `OctPlugins/kernel/`。
- 审阅并修正 `cmd/kerneld/main.go` 中项目根/plugins/resources 解析逻辑，使其指向 `OctPlugins/../plugins`、`../resources`、`../state`、`../config`（与架构 §18 目录一致）。
- 保留 `127.0.0.1:0` 自选端口 + stdout 首行 `{auth:{port,token}}` 协议（架构 §2.1.3、§11.5）。
- 用 `go build -o kernel/kerneld.exe ./cmd/kerneld` 验证可编译。

### 2. 迁入宿主 UI 并适配 `ui/`
- 复制 `OCTplugin/host/index.html`、`main.js`、`package.json`、`.npmrc` 到 `OctPlugins/ui/`。
- 在 `main.js` 中调整路径常量：
  - `KERNEL_EXE` → `path.join(__dirname, "..", "kernel", "kerneld.exe")`
  - `APP_LOGO` / 托盘图标 → `path.join(__dirname, "..", "resources", "logo", …)`
  - `PLUGINS_ROOT` / `HOTKEY_STORE` 等 → 对齐 `../../plugins/`、`../../state/`。
- `index.html` 中 logo 相对路径（当前 `../resources/logo/…`）因层级变化改为 `../../resources/logo/…`。
- 保留渲染进程 `ipcRenderer.invoke("rpc", …)` 桥与 `kernel:ready`/`kernel:event` 事件流（架构 §17.3：UI 只发意图）。

### 3. 准备示例插件
- 迁入 `home` 插件（web UI + manifest）并核对 `manifest.json` 的 `entry`/`ui.entry` 与内核期望一致。
- 可选再迁入一个轻量 `demo_web`，验证多插件侧栏。
- 进入首页会触发 inner `plugin.start` 与 iframe `postMessage("oct.hello")` 握手，核对端口/token 注入。

### 4. `config/`、`state/` 占位
- 建目录并放最小结构说明（如 `.gitkeep` 或空占位 `user-settings.json`/`registry.json`），使内核首启不报路径缺失。

### 5. 一键构建脚本 `build.ps1`
- `go build` 内核 + `npm install` + 提示 `npm start`。

## 验证（端到端）
1. 在 `OctPlugins/kernel` 执行 `go build` 成功，产出 `kerneld.exe`。
2. 在 `OctPlugins/ui` 执行 `npm install && npm start`：窗口显示「连接中…」→ 内核连上转「已连接」。
3. 侧栏出现 `home`（及可选 `demo_web`）插件；状态点按内核真实 State 着色；点击插件面板加载 iframe 并完成 `oct.hello` 握手，插件内容正常渲染。
4. 顶栏时钟/窗口控制/logo 折叠侧栏/托盘/最小化均正常。
5. 设置面板（可先用「插件列表 / 启动下载策略」等无需外部模型的项）能与内核 rpc 交互，右侧结果显示无报错。

## 不在本期范围
- 架构《理想架构.md》中 P2 的大模型能力层、跨插件共享函数（一期非必须，若参考实现已含则随迁保留）。
- 完整的 `settingsSchema` 驱动渲染（参考实现为主进程手写表单，符合架构 §17.2-B「MAY 手写」）。
- 重构/拆分参考实现那 100KB 单文件 `index.html`（保持迁移一致性优先）。