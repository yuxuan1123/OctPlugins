# napi_rs_tool 架构重构计划（含进程管理复用）

## Context（为什么做这件事）

当前取色器把 napi-rs 原生模块加载（`findColorpickerNode`）、overlay 放大镜、取色状态机、IPC handler **全部硬编码在宿主** `ui/src/main.ts`（`cpStart`/`cpOnEvent`/`createColorpickOverlay`/`colorpick:*`），违背三层架构。

用户要求重构为：在 `mvp_test/` 新增独立 **`napi_rs_tool`** 管理 napi-rs；宿主「添加插件」下新增 **Tools** 入口；宿主只调起 tool + 提供 IPC 通道，**零硬编码**；为 tool 写**详细接口文档**。

**新增关键要求（本次修订）**：tool 与插件一样要有**完整进程管理**，且**直接复用插件的进程管理机制**——启动方式（常驻/按需/空闲回收）、关闭逻辑、崩溃退避重启、心跳看护等，而不是走现有简陋的 `resources/map.go` 引用计数。

## 内核调研结论（决定复用方式）

`kernel/internal/lifecycle/manager.go` 已提供插件级、可直接复用的进程管理：
- **状态机**：`stEvCh` 单 goroutine `stateLoop`，`opStart/opExit/opRecycle/opStop/opStopAll/opRestart`，对外 `post`/`fire`（`manager.go:865-941`）。
- **启动**：`loopSpawn`（`manager.go:983`）→ `Plugin.Start`（`plugin.go:117`）`exec.Command` + `process.PreStartAttrs` 隔离 + `attachLifecycle` Job Object。
- **空闲回收**：`patrol`（`manager.go:1066`）`recycleTicker` → `recycleDue`（`recallSweep:1137`），onDemand/idleTimeout。
- **崩溃退避**：`loopExit`（`manager.go:1202`）+ `nextBackoffLocked`（`manager.go:1307`），`AutoRestart/MaxRestarts/BackoffBase` 指数退避。
- **优雅/强制关闭**：`Plugin.Stop`（`plugin.go:612`）握手后 `shutdown` 等 `GracefulShutdownMs`，超时 `process.KillTree`。
- **握手/看护**：`attemptHandshake`（`plugin.go:227`）+ `healthSweep`（`manager.go:1092`）。
- **插件专属 vs 通用**：manifest/RegState/LoadMode/interp/gate/perms/GetOrStart 是插件专属；**状态机骨架、空闲回收、崩溃退避、优雅关闭、握手看护完全通用**，可直接复用于 tool。

## 复用策略（关键决策）

**方案：tool 本质是「无 UI 的插件实体」，走插件完整通道复用进程管理（状态机/空闲回收/崩溃退避/优雅关闭/看护），不新建第二套进程管理。**

用户确认：tool 在我设计中就是无 ui 的插件，因此有：
- **完整复用** manager 的 `RegState` 状态机、`loopExit` 崩溃退避、`patrol`/`recycleDue` 空闲回收、`Plugin.Stop` 优雅/强制关闭、`attemptHandshake` 看护。
- **外显差异**：侧栏**不展示** tool；设置里**可配置** tool（与插件分开或合并展示均可，推荐同列表但 tool 节点无 UI/角标区分）。

为何不直接用 `resources/map.go`：它只有 `Acquire/Release` 引用计数 + `spawn/stop` 钩子，无状态机/退避/回收/看护。故 tool 走插件进程通道，`resources` 层仅作 tool 的登记与依赖声明协同。

### 内核改动（谨慎、收敛在标记与过滤，不动核心状态机）
1. **tool 标识**：manifest 增加 `kind: "tool"`（可选，默认 `app`）。`Manifest` 结构新增 `Kind string` 字段（`manifest.go`）。tool 与 plugin 共用注册/加载/启停全通道。
2. **无 UI 断言**：tool 的 `manifest.ui` 允许缺省（本就是可选 `omitempty`）；tool 注册时校验无 `ui`，否则告警（非阻塞）。
3. **侧栏过滤**：`plugin.list` 返回新增 `kind` 字段；宿主侧栏 `renderPlugins` 仅渲染 `kind!=="tool"`；tool 不出现在数据源。`plugin.list`（宿主管控）仍可见 tool（供设置页/管理用）。
4. **设置页**：`settings` 场景下列出 tool（含启动策略/停止策略/常驻 onAppStart），与插件同表单体系。
5. **tool 进程构造**：仍是 `exec.Command`（node 解释器 → `main.js`），Dir=tool 根，环境注入复用 `process.BuildPluginEnv`（`OCT_CHANNEL_TOKEN` 等），`attachLifecycle` Job Object。协议走插件同一 stdio JSON-RPC + 握手。

> 改动边界：收敛在 manifest 的 `Kind` 字段 + `plugin.list` 透传 + 宿主侧栏/设置过滤。**不进 resources/map.go 的引用计数逻辑、不动 manager.go 状态机**。回归：home/mdeditor 无改动仍正常启停/崩溃回收。

## 一、新建 `mvp_test/napi_rs_tool/`
```
mvp_test/napi_rs_tool/
├── tool.json         # ID、node 入口、startPolicy/stopPolicy、管理哪些 .node、方法签名清单
├── main.js           # Node 常驻：require(.node)、stdio JSON-RPC 服务 load/call/subscribe/unload
└── DESIGN.md         # 详细接口说明（供其他插件调用）
```
### 泛化 RPC（stdio 新行 JSON）
- `ping` → `{ok,ts}`
- `load` `{modulePath}` → `{handle}`
- `call` `{handle,method,args[]}` → `{result}`（取色：`call(handle,'captureRegion',[x,y,w,h])`）
- `subscribe` `{handle,eventName}` → `{ok}`（取色事件 `cursor/rightclick/double/hotkey` 经此回吐宿主→插件）
- `unload` `{handle}` → `{ok}`
- `list` → 已载模块 + 方法

## 二、宿主改造（零取色硬编码）
1. 删 `ui/src/main.ts` 全部 `cp*`/`findColorpickerNode`/`colorpick:*` IPC。
2. 通用桥 IPC：`tool:list`、`tool:invoke{toolId,method,params}`、`tool:event{toolId,event}`；宿主经 WS→内核→tool 进程转发，**不含取色逻辑**。
3. `ui/index.html` 「添加插件」下新增 **Tools** 分类（import tool 目录、列出/移除、查看接口说明）。
4. `renderer.ts`：`oct.tool.invoke` + `tool:event` 回吐插件 iframe。
5. overlay 放大镜：迁移为宿主通用「ToolOverlay」能力（`tool:overlayOpen/overlayData/overlayClose` IPC），tool 提供像素、宿主透明窗渲染——仍是通用能力，非取色硬编码。

## 三、colorpicker 改为消费端
- `plugins/colorpicker/manifest.json` 声明 `externalDependencies:[{id:"napi_rs_tool",kind:"tool"}]`。
- 经 `oct.tool.invoke("napi_rs_tool",...)` + `subscribe` 取色；保留 Go 后端：UI + 颜色历史持久化。

## 四、DESIGN.md 接口文档
在 `mvp_test/napi_rs_tool/DESIGN.md`：tool.json schema、错误码、每个 RPC 请求/响应示例、事件 payload JSON 样例、新增 `.node` 到 tool 的步骤。

## 验证
1. `ui` `npx tsc` 通过；内核 `go build` 通过；`colorpicker.node`/`colorpicker.exe` 编译。
2. 导入 napi_rs_tool 与 colorpicker，Tools 面板见 tool；tool 常驻、可 kcill/恢复（崩溃退避）。
3. 插件「开始取色」→ tool 事件转发 → 放大镜显示、右键复制、双击/Ctrl+. 退出。
4. 回归：home/mdeditor 插件启停、崩溃回收不受影响。