# 取色器端到端打通计划（overlay 作为独立插件实体）

## Context（为什么改）
`mvp_test/colorpicker` 各子模块（napi-rs native、napi_rs_tool 工具进程、Go 历史后端、管理页）已建但**端到端未打通**，取色器实际无法运行：
- 放大镜页从未被任何窗口加载 → 全桌面放大镜不出现。
- `native/lib.rs` 未导出 `setCallback`、无人调 `start()` → 钩子/热键不启动、事件不发出。
- 内核 `event.emit` 只广播宿主主 WS（已实测），宿主未把 tool 事件转给放大镜窗口 / 插件 iframe。
- 交互语义（右键复制/双击右键退出/ctrl+. 启停/左键穿透）未落地。

**本次确定的设计取向**：与 `kind:"tool"` 对称，把**放大镜 overlay 做成独立插件实体**（`kind:"overlay"`）。宿主只新增**通用**能力（按 kind:overlay/pluginId 开全屏透明窗、按 source 转发 tool 事件），取色编排全在 colorpicker 插件内，宿主零取色硬编码。

## 已确认的基础事实（源码核实）
- `plugin.call` 可调用 kind:"tool"；`event.emit` → 只广播宿主主 WS（`{method:"event",params:{source,type,data}}`），不广播 iframe。
- `plugin.start` 已暴露，**无 `plugin.stop`**；WS **无客户端角色鉴权**（插件 iframe 也能调 `plugin.start`）。
- `Manifest.Kind` 不参与进程校验；`kind:"overlay"+type:"node"+minimal main.js+ui` 会被内核正常接受（内核不感知 kind 语义，UI 载体由宿主自行开窗）。

## 架构与责任划分
**实体**：
- `napi_rs_tool`（kind:"tool"）：承载取色 `.node` 的进程。
- `colorpicker_overlay`（kind:"overlay"，**新增独立插件**）：放大镜页的宿主载体与身份；进程侧是极小 node stub，真正工作的是其 `ui/overlay.html` 由宿主开成全屏透明窗。
- `colorpicker`（kind 默认）：管理页 + Go 历史后端 + **编排者**。

**数据流（live）**：
native `emit()` → 内层 JSON 串 → tool `setCallback` stub→`event.emit` → 内核广播宿主 `{method:"event",params:{source:"napi_rs_tool",type:"colorpick",data:"<内层串>"}}`
→ 宿主 `ws.on("message")` 非应答分支 **双投**：
  (a) 推给已开的 overlay 窗口（`overlay:event`，宿主直连）；(b) 原样发 renderer `kernel:event`。
→ renderer：对订阅了该 toolId 的 **colorpicker iframe** `forwardToPlugin` → `oct.tool.event`（**跳过 cursor 类型**，防刷屏）。

内层 JSON 实例：
- cursor：`{"type":"cursor","x":512,"y":300,"hex":"#3d7a2e","region":"<base64·21×21×4≈2352字符>"}`
- rightclick：`{"type":"rightclick","x":..,"y":..,"hex":"#3d7a2e"}`
- double：`{"type":"double","x":..,"y":..}`；hotkey：`{"type":"hotkey"}`；ready/error 原样。
像素用 **base64**（经 tool stringify+内核 JSON+send，裸字节含 0x00 会 `\u0000` 膨胀）。16ms 节流 ≈140KB/s 可接受，卡则提到 30ms。

职责：
- **overlay 窗口**：收 `overlay:event`，渲染放大镜 pixel + hex + 暗金十字/色片；rightclick/double 自显 toast。纯显示。
- **colorpicker iframe**：收 `oct.tool.event`（非 cursor）：rightclick→clipboard+history.add；double/hotkey→tool stop+关 overlay+复位 UI。

## 改动清单（按顺序）

### 1) native —— `mvp_test/colorpicker/native/src/lib.rs` + `Cargo.toml`
- 导出 `#[napi(js_name="setCallback")] pub fn set_callback(&mut self, cb: ThreadsafeFunction<String>)` → `CALLBACK.set(cb)`（满足 napi_rs_tool subscribe 接管）。
- `start(&mut self, cb: Option<ThreadsafeFunction<String>>)`，仅 `Some` 覆盖 CALLBACK。
- cursor 分支（节流内）在原鼠标位捕获 21×21(负坐标 clamp)，base64 + 中心 hex 进事件串；rightclick 补 hex。
- 删除 `WM_LBUTTONDOWN` 分支（左键穿透由宿主 `setIgnoreMouseEvents` 承担，不再发 leftclick）。
- `Cargo.toml` 加 `base64 = "0.21"`。
- 重编：`mvp_test/colorpicker/native/build.bat` → `ui/build/colorpicker.node`。

### 2) 新增 overlay 插件实体 —— `mvp_test/colorpicker_overlay/`
- `manifest.json`：`{"id":"colorpicker_overlay","name":"取色放大镜","version":"0.1.0","kind":"overlay","type":"node","entry":"main.js","ui":{"type":"web","entry":"ui/overlay.html"},"load_mode":"lazy","permissions":[],"dependencies":[],"commands":[],"functions":[]}`（类型 node 仅当身份/进程占位，实际由宿主开 UI 窗）。
- `main.js`：极简 node stub（`$/handshake` + ping/shutdown），保持插件进程模型一致。
- `ui/overlay.html`：采纳原 `colorpicker/ui/colorpick-over.html` 的放大镜实现，改监听 `overlay:event`（宿主直推，`JSON.parse(data)`）：cursor→解码 base64 region 为 `ImageData` 铺 canvas + 定位 + 色片；rightclick→toast「已复制 #hex」；double/hotkey→退出提示。
- 移除原 `mvp_test/colorpicker/ui/colorpick-over.html`（内容已迁移）。

### 3) 宿主 main.ts —— `../ui/src/main.ts`
- 模块级 `const overlayByPlugin=new Map()`（pluginId→BrowserWindow）。
- `ipcMain.handle("overlay:open",(e,{pluginId})=>`：从 `rpc("plugin.list")` 找该 plugin 的 `ui.web.entry` → 解析绝对路径 → 新建 `BrowserWindow`：`transparent:true, frame:false, alwaysOnTop:true, skipTaskbar:true`，取 `screen.getPrimaryDisplay().bounds` 全屏，`setIgnoreMouseEvents(true)`，`nodeIntegration:true, contextIsolation:false`（overlay.html 用裸 `require`）；登记 `overlayByPlugin`，`closed` 清表。
- `ipcMain.handle("overlay:close",(e,{pluginId})=>`：关窗 + 清登记。
- **改 `connect()` 内 `ws.on("message")`（main.ts:552-555）非应答分支**：`const p=msg.params||{}`；若 `overlayByPlugin.get(p.source)` 命中则 `o.webContents.send("overlay:event", JSON.stringify(msg))`；随后仍 `win.webContents.send("kernel:event", …)`。二者并行。
- `app.on("will-quit")` 遍历 `overlayByPlugin` 强关。

### 4) 宿主 renderer.ts —— `../ui/src/renderer.ts`
- `renderPlugins` 侧栏过滤：`kind!=="tool" && kind!=="overlay"`（扩充现 tool 过滤处）。
- 新增 `const toolSubs:Map<toolId,Set<pid>>`。
- message 监听新增：`oct.tool.subscribe{toolId}`（按 ev.source 对应 iframe `data-plugin` 记入 toolSubs）；`oct.overlay{action:"open"|"close",pluginId}` → `ipcRenderer.invoke("overlay:open"/"overlay:close",{pluginId})`。
- `kernel:event` 处理器（现 line 1175）：`p.source!=="kernel"` 时对 `toolSubs.get(p.source)` 各 pid `forwardToPlugin(pid,{type:"oct.tool.event",source:p.source,dataType:p.type,data:p.data})`，`dataType==="colorpick" && data.内层.type==="cursor"` 跳过。

### 5) 插件 app.js —— `mvp_test/colorpicker/ui/app.js`
- 订阅登记：请求后 `window.parent.postMessage({type:"oct.tool.subscribe",toolId},"*")`。
- `startPick()` 顺序：`oct.overlay`开(colorpicker_overlay) → `load` → `subscribe(handle,"colorpick")`（触发 tool 接管 setCallback）→ 正在 `call start`。subscribe 先于 start。
- 收 `oct.tool.event`：`JSON.parse(data)`；rightclick→clipboard+`history.add`+刷新+setStatus；double/hotkey→`call stop`+`oct.overlay`关+`onEnded`。
- `stopPick()`（供按钮/Ctrl+. 兜底）补 `call stop` 再 unload。

### 6) napi_rs_tool —— 无需改动
`subscribeModule` 已探测 setCallback 并接管为 `event.emit`；确认调用方按 subscribe→start 顺序即可。

## Verification（端到端）
1. 编 native：`mvp_test/colorpicker/native/build.bat` → `ui/build/colorpicker.node` 更新。
2. 部署：`mvp_test/napi_rs_tool/`、`mvp_test/colorpicker/`、`mvp_test/colorpicker_overlay/` 作为同级插件导入宿主（相对路径 `../colorpicker/ui/build/colorpicker.node` 成立）。
3. 宿主 `npx tsc` 通过。
4. 运行 → 侧栏「取色器」→「开始取色」：全屏透明放大镜出现并实时跟随整个桌面光标，8× 放大 + 暗金十字 + hex 色片。
5. 桌面任意点：左键穿透（不取色）；右键复制当前 hex + 历史新增 + overlay「已复制」toast；双击右键 / Ctrl+. 退出、按钮复位、overlay 关闭。
6. 取色器页历史含刚才 hex，可复制/清空。
7. 回归：宿主退出无残留 overlay。

## 边界与风险
- base64+16ms ≈ 百KB/s 跨 stdio+WS；卡则 native 节流提到 30ms。
- 非 overlay sidebar：kind 在 {tool,overlay} 均不进侧栏，管理面板仍可见。
- overlay 页纯显示不写库，历史只由 colorpicker 写，避免重复。
- 系统悬浮层（IME/OSD）上渲染透明置顶窗可能被 DWM 降级，不影响主交互。