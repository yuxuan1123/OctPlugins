# 全局取色器（napi-rs 工具 + Electron overlay）

## Context（为什么做）

在 `mvp_test` 下新增一个**全局系统级取色器**：跨整个桌面取色（放大镜跟随光标），并作为宿主加载的「工具」与插件通过既有信道沟通。内核已具备 `kind=tool` 资源抽象（定位/登记/Acquire），宿主已具备 `postMessage ↔ renderer ↔ ipcRenderer ↔ main` 的插件→宿主桥，本功能复用它。

已与用户确认：
- **放大镜形态**：Electron 无边框、透明、置顶、鼠标穿透的 overlay 窗口（复用宿主）。
- **入口**：像 home 插件一样的一张插件页/卡片，点「开始取色」进入全屏取色态。
- **交互**：一键进入全屏；左键**穿透**（可正常点击/切换应用）；右键复制取到的 HEX；双击右键退出；全局热键 `Ctrl+.` 切换进入/退出；页面同步展示取到颜色，并**持久化历史**。

## 关键架构决策

napi-rs 产出的是 `.node` 原生模块，是被 Electron 主进程 `require()` 的链接库，**不是**内核 `tool.go` 里 `Spawn()` 起的独立进程。因此本功能里的「工具」= 宿主主进程加载的 `.node` 模块；插件↔工具的沟通 = 既有 postMessage 桥 + 新 IPC 通道。内核 `kind=tool` 的资源登记/定位仍用于「确认 native 模块在不在、可复用 Acquire」，但不走进程 Spawn。

### 分层

| 层 | 职责 |
|---|---|
| Rust napi-rs 模块（`colorpicker.node`） | 全局 `WH_MOUSE_LL` 鼠标钩子、`RegisterHotKey`(Ctrl+.) 、屏幕区域像素采样、光标取色 |
| Electron 主进程（`ui/src/main.ts`） | `require()` 该 `.node`；持有 overlay 窗口 + 全局钩子生命周期；把取到的颜色经 IPC 事件推给 renderer；写剪贴板 |
| Renderer（`ui/src/renderer.ts`） | 转发插件 iframe 的 `oct.colorpick:*` postMessage ↔ ipcRenderer；把 `colorpick:color` 事件回吐到 iframe |
| colorpicker 插件（Go，像 home） | manifest 声明 `externalDependencies kind=tool id=colorpicker` + `UIDecl type=web`；后端提供 `pick.start/stop/history.add/list` RPC；历史写本地 `color_state.json` |

## 实施步骤

### 1. Rust napi-rs 模块
新目录 `mvp_test/colorpicker/native/`（Cargo crate，`cdylib` + `napi` / `napi-derive`），产出 `colorpicker.node`。

暴露给主进程的导出：
- `startPicking()` / `stopPicking()`：安装/卸载全局钩子（`WH_MOUSE_LL` 检测右键单击/双击，`RegisterHotKey` ctrl+.）。
- `captureRegion(x, y, w, h)` → RGBA Buffer：供放大镜拉桌面某区域像素。
- `getPixel(x, y)` → `{r,g,b}`：取单个像素。
- 事件回调（napi `ThreadsafeFunction` 回到 JS）：`onRightClick()`、`onDoubleRightClick()`、`onHotkey()`、`onCursor(x,y)`。

依赖：`napi`（最新 2.x）、`napi-derive`；Windows 下用 `windows` crate 的 `SetWindowsHookExW`、`RegisterHotKey`、`GetDC`/`BitBlt`。跨平台守卫：非 Windows 返回不可用（本阶段仅 Windows 实测）。

构建脚本 `mvp_test/colorpicker/native/build.bat`（ASCII，沿用 home 的编码教训）：
```
cargo build --release
copy /Y target\release\colorpicker.dll ..\..\..\..\ui\build\colorpicker.node   // 注意相对路径与产物命名
```

### 2. Electron 主进程（`ui/src/main.ts`）
- 顶部 `const colorpick = safeRequire(path.join(__dirname, "colorpicker.node"))`（存在才加载，失败不阻塞启动）。
- overlay 窗口工厂 `createColorPickerOverlay()`：`new BrowserWindow({frame:false, transparent:true, alwaysOnTop:true, resizable:false, hasShadow:false, skipTaskbar:true, webPreferences:{contextIsolation:true}})`，加载一个内嵌的 over.html（放大镜渲染），`win.setIgnoreMouseEvents(true, {forward:true})` 实现左键穿透。
- 主进程持有全局取色状态机：`startPick()`（显示 overlay、`colorpick.startPicking()`、每 ~16ms `await captureRegion(cursor) → win.webContents.send` 推放大镜帧）+ `emitColor(hex)`（写剪贴板、`win.webContents.send("colorpick:color",{hex})`）+ `stopPick()`。
- IPC：
  - `ipcMain.handle("colorpick:start"/"colorpick:stop")`（renderer 转插件请求进入/退出）。
  - `ipcMain.on("colorpick:result", ...)` 预留：native → JS 回调再 `webContents.send`。
  - 单例：`colorpick.active` 与窗口都只在一个，重复 start 幂等。

### 3. Overlay 放大镜页（`ui/build` 或宿主资源目录内 `colorpick-over.html`）
- 全屏透明、pointer-events:none。
- 跟随光标：主进程每帧 send `{x,y,buffer,w,h,zoom}`，canvas 放大绘制，中心画暗金色十字（外圈+中心点，符合既有视觉偏好）。
- 尺寸：覆盖光标周围放大区，位置贴光标但不遮挡中心取色点。

### 4. Renderer 桥（`ui/src/renderer.ts`）
- 在既有 message 处理器（`oct.openPluginWindow`/`oct.dialog` 同段）新增：
  - `oct.colorpick.start / stop` → `ipcRenderer.invoke("colorpick:start"/"colorpick:stop")`（来源校验同上）。
  - 监听主进程 `colorpick:color` → 解析出当前插件 iframe source，`ev.source.postMessage({type:"oct:colorpick:color",hex}, "*")` 回吐给插件页。

### 5. colorpicker 插件（Go，像 home）
结构（对齐 `mvp_test/home` 工程约定，含 build.bat、go.mod、main.go、ui/、store/、manifest.json）：
- `manifest.json`：`type:"Go"`、`entry:"colorpicker.exe"`、`permissions`、`dependencies`、`commands`、`functions`、`ui:{type:"web",entry:"ui/index.html"}`、`load_mode`、**新增 `externalDependencies:[{id:"colorpicker",kind:"tool",locate:"bundled"}]`** 走内核资源登记。
- Go 后端 RPC：`pick.start`/`pick.stop` 转发宿主信号（经内核 `plugin.call` 到宿主能力，或直接由插件 UI postMessage 桥调用宿主）；`history.add(hex)`/`history.list()` 读写 `store/color_state.json`（RWMutex + 原子写，复用 home 的 home_state.json 模式），最多存 20 条 HEX。
- `ui/index.html`：首页卡片（高质感白/碳灰，暗金按钮），含「开始取色」按钮、当前色预览、历史色板（每块可点击复制）。
- `ui/app.js`：复用 home 同构桥（postMessage 接 `oct.hello` 连内核 WS）；「开始取色」→ 经 `toParent({type:'oct.colorpick.start'})` 调宿主；监听 `oct:colorpick:color` 更新页面 + `history.add`。

### 6. 构建顺序
native `.node` → host tsc 重编译（main/renderer）→ Go 插件 build.bat → 宿主里导入 colorpicker 插件。（沿用记忆：GOARCH=amd64、CGO_ENABLED=0、GOTOOLCHAIN=local。）

## 复用点
- 插件─宿主桥：`ui/src/renderer.ts` 阶段G message 处理器 + `ui/src/main.ts` 的 `win:openPluginWindow`/`dialog:pickDir`/`win:ctrl` 现成 IPC 模式。
- 全局热键宿主侧注册：`main.ts` `applyHotkeys()`/`globalShortcut` — 但全屏取色需原生 `RegisterHotKey`（overlay 置顶但焦点可能不在宿主），故本功能用 native 的全局热键而非 `globalShortcut`。
- 状态文件原子写：参考 `mvp_test/home/main.go` 的 `home_state.json` + RWMutex 模式。
- 插件目录工程结构：复用 `mvp_test/home` 骨架。

## 验证
1. `mvp_test/colorpicker/native/build.bat` 编译出 `ui/build/colorpicker.node`（cargo 成功 + 文件存在）。
2. `ui` 下 `npx tsc` 通过（main/renderer 新 IPC）。
3. Go 插件编译 `colorpicker.exe`；启动宿主导入插件，首页见到「取色器」卡片。
4. 端到端（手动）：点「开始取色」→ 全屏出现跟随光标的放大镜（暗金十字对准中心）→ 移动光标取色 → **单击右键**＝复制当前 HEX（粘贴验证）→ **双击右键**＝退出取色 → `Ctrl+.` 进入/退出反复验证 → 页面出现历史色板，重启宿主后历史仍在。

## 风险与边界
- `.node` 与 Electron 版本 napi ABI：需用 `napi`（Node-API v3+，ABI 稳定），不依赖 V8 版本，规避 version mismatch。
- 全局钩子在主进程消息循环线程安装（Electron main 线程有 pump），避免 dll-less WH_MOUSE_LL 在无 pump 线程失效。
- 尚未安装 `node-modules`/原生编译链则由 build.bat + cargo 现成产物兜底，缺失时报明确 E_TOOL_UNAVAILABLE。