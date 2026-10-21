# OctPlugins 全盘问题检测报告

> 扫描范围：仓库根目录全部文件（kernel 61 个 Go 文件、ui 4 个 TS + index.html、sdk/node + sdk/python、tools 4 个脚本、build/package/start 脚本、resources、根目录散落文件、文档）。
> 扫描方式：逐文件逐行阅读 + git 状态核对 + 跨端一致性比对（Go 内核 / Electron 宿主 / Node SDK / Python SDK / 生成物）。
> 扫描日期：2026-10-13。
>
> 分级说明：
> - **P0 阻断性**：导致编译失败、核心功能不可用、或安全/数据正确性受损，必须优先修。
> - **P1 冲突/不一致**：两端/多端行为或声明互相矛盾，代码能跑但结果是错的或误导的。
> - **P2 奇怪代码/坏味道**：死代码、假实现、乱码注释、命名错误、潜在竞争等。
> - **P3 架构/工程化**：结构性设计问题、文档与实现漂移、仓库卫生。

---

## 一、文件功能清单（每个文件一句话）

> 格式：`文件 — 功能`，`[问题编号]` 表示该文件存在问题，编号见第二节。

### 1. kernel（Go 内核）

| 文件 | 功能一句话 |
|---|---|
| `kernel/cmd/kerneld/main.go` | 内核可执行入口：取二进制所在目录的上一级为项目根，调用 `app.Run` 装配并阻塞运行。[P2-21] |
| `kernel/cmd/protocolgen/main.go` | 代码生成器：从 `pkg/protocol.ErrorTable` 生成 `errors.json` 与 Node/Python/TS 三份错误码常量，杜绝三端硬编码。[P1-5] |
| `kernel/internal/app/app.go` | 装配根：注入 manager/ipcserver/config/registry/runtime/resources，监听信号做退出清理，向 stdout 打印 auth 行交宿主。 |
| `kernel/internal/config/config.go` | `user-settings.json` 原子读写 + 插件设置冲突消解（user→defaults→内置）+ `settingsSchema` 字段白名单提取。[P2-56] |
| `kernel/internal/ipcserver/ws.go` | 内核 WS 服务端：token 认证、约 40 个 RPC 方法分发、事件广播、`/res/` 与 `/plugin/` HTTP 静态服务（含目录穿越防护）。[P1-14][P1-15][P2-20][P2-21][P2-22][P3-66] |
| `kernel/internal/lifecycle/capability.go` | 能力维度模型解析（§15.4 链：用户 pin>作者默认>量化变体>fallbacks>宿主兜底）与模型路径/显存适配。 |
| `kernel/internal/lifecycle/contract.go` | manifest §23 契约登记期校验（spawn 白名单对得上 externalDeps、net 与 network 权限一致、能力查重）。[P2-28] |
| `kernel/internal/lifecycle/download.go` | 模型首次下载（§26）：HTTP 拉取到 `<modelsRoot>/<id>/<quant>/`、`.part` 临时文件、sha256 校验、200ms 节流进度事件。[P2-64] |
| `kernel/internal/lifecycle/ensure.go` | `gate.model_ensure` 宿主侧实现：解析能力→确保就绪→handover 窗口内等待/降级，pin/override 目标不降级为快模型。 |
| `kernel/internal/lifecycle/handover.go` | 双模型 handover：显存账先算、并行窗口/串行切换、grace 截止镜像、成败事件广播。 |
| `kernel/internal/lifecycle/layout.go` | 模型存储布局审计（只读）与搬迁（dryRun→confirm，目标不覆盖、源须在模型根内）。[P2-65] |
| `kernel/internal/lifecycle/lifecycle.go` | 生命周期策略数据结构：load_mode/heartbeat/recycle/resilience/limits 的默认值、校验、triggers 声明。[P2-58][P2-59][P2-60] |
| `kernel/internal/lifecycle/manager.go` | 插件生命周期核心：发现/登记、状态机单 goroutine（start/exit/recycle/stop/restart/stopall）、退避重启、健康巡检、共享函数表、依赖安装 gating、registry 写入、模型/资源装配。[P1-13][P2-29][P2-69] |
| `kernel/internal/lifecycle/manifest.go` | manifest.json 声明式 schema：权限/依赖/命令/函数/热键/profiles/codeDeps/externalDeps/settingsSchema/conversions/providesModels/nativeLibs/§23 契约字段。 |
| `kernel/internal/lifecycle/model_hooks.go` | 模型加载钩子：经 `plugin.call` 向 provider tool 下发 `model.ensure`/`model.release`（权重路径由宿主下发）+ 伴随模型路径聚合。 |
| `kernel/internal/lifecycle/models_api.go` | 面向宿主的模型管理 API：全量快照（pinned/effective/unavailable 标注）、acquire/release、单条查询。 |
| `kernel/internal/lifecycle/nativelib.go` | 原生共享库（.dll/.so）spawn 前 sha256 校验（§20.4），哈希不匹配拒绝启动、optional 仅容忍缺失。 |
| `kernel/internal/lifecycle/outtools.go` | `outtools.json` 描述清单加载与登记：outtool 的 providesModels 进 registry.models 与 ResourceMap（先到先得）。 |
| `kernel/internal/lifecycle/override.go` | 用户生命周期覆盖（`state/overrides.json`）的持久化、字段校验与应用（override 优先 manifest）。 |
| `kernel/internal/lifecycle/plugin.go` | 单插件进程实例：spawn、NDJSON 读写循环、$/handshake 认证、gate 请求处理（file/exec/registry.call/model/settings/perm_assert/busy）、Call/Ping/Stop/Kill 与退出回收。[P2-23][P2-24][P2-42] |
| `kernel/internal/lifecycle/resources.go` | §14 ResourceMap 与生命周期装配：登记 externalDependencies、spawn 前 Acquire、停止/崩溃/退出后 Release、registry 资源节落盘。[P2-61] |
| `kernel/internal/lifecycle/jobobj_windows.go` | Windows Job Object 装配：KILL_ON_JOB_CLOSE + 可选内存上限，严格 CREATE_SUSPENDED→Assign→Resume 时序。 |
| `kernel/internal/lifecycle/jobobj_other.go` | 非 Windows 空实现（接口对齐）。 |
| `kernel/internal/lifecycle/stdproc_windows.go` | Windows 隐藏控制台窗口 + CREATE_SUSPENDED 派生。 |
| `kernel/internal/lifecycle/stdproc_other.go` | 非 Windows 空实现。 |
| `kernel/internal/lifecycle/contract_test.go` 等 14 个 `*_test.go` | 对应模块单元测试：契约/下载/gating/握手/布局/manifest/nativelib/outtools/override/状态机/篡改/生命周期。[P2-28] |
| `kernel/internal/perms/perms.go` | 权限位图引擎：声明/授权/吊销/检查、`state/perms.json` 0600 持久化、高危权限标记。[P1-10][P2-40][P2-76] |
| `kernel/internal/process/env.go` | 插件子进程环境组装：剔除污染型变量（PYTHONVERBOSE/NODE_OPTIONS 等）+ 注入 OCT_CHANNEL_TOKEN/OCT_PLUGIN_HOME/OCT_PLUGIN_DATA/PYTHONPATH(sdk)。 |
| `kernel/internal/process/spawn.go` | 通用子进程派生（协议管道 + Job 挂接 + fd3 日志），当前仅被 `resources.Spawn` 间接使用。[P2-74] |
| `kernel/internal/process/spawn_windows.go` | Windows 平台属性：CREATE_SUSPENDED 派生 + attachJob（Assign 后 Resume）。[P2-74] |
| `kernel/internal/process/spawn_unix.go` | Unix 平台属性：Setpgid 进程组隔离 + attachJob 空实现。 |
| `kernel/internal/process/kill_windows.go` | Windows 进程树终止（Kill 主进程，Job 连带回收整树）。 |
| `kernel/internal/process/kill_unix.go` | Unix 进程树终止（向进程组发 SIGTERM）。 |
| `kernel/internal/registry/registry.go` | `registry.json` 唯一写入方：插件/资源/模型条目原子读写（Mutate 事务）与 schemaVersion 迁移链。[P1-12][P2-61] |
| `kernel/internal/resources/map.go` | ResourceMap：资源登记/引用计数/能力维度上限（默认 1，grace 期 2）/停止策略/StopAll。[P1-26][P2-25][P2-74] |
| `kernel/internal/resources/model.go` | kind=model 条目：ModelHandle、宿主侧 Ensure/Release 钩子注入（§四 加载权：tool 只执行不自行加载）。 |
| `kernel/internal/resources/tool.go` | kind=tool 条目：三选一定位（bundled/explicitPath/env + 注册表 + 常见路径兜底）与 spawn 钩子、通用 `Spawn`。[P1-26][P2-74] |
| `kernel/internal/resources/tool_windows.go` | Windows 注册表探测 LibreOffice 安装路径（env 定位兜底）。 |
| `kernel/internal/resources/tool_other.go` | 非 Windows 空实现。 |
| `kernel/internal/resources/discover.go` | 探测本机常见运行时（python/node/uv/ffmpeg/ollama…）是否在 PATH。[P2-27] |
| `kernel/internal/runtime/runtime.go` | 依赖安装器：venv 解析、镜像源/缓存配置持久化、BundledPython 探测；`Install/Ready/Preview/ListPackages` 为骨架占位。[P0-4][P2-75] |
| `kernel/internal/transport/deframe.go` | NDJSON 按行切帧 + 逐行即时 JSON 校验（污染即返回错误）+ 超长行切断告警。 |
| `kernel/internal/transport/handshake.go` | `$/handshake` 帧解析与校验（token/protocolVersion，params 优先、兼容顶层旧格式）。 |
| `kernel/internal/transport/ndjson.go` | NDJSON 行序列化/校验小工具。[P2-73] |
| `kernel/internal/watchdog/watchdog.go` | 巡检器：心跳超时/busy 僵死判定 `Tick` 与独立 `Run` 循环。[P2-39][P2-71] |
| `kernel/pkg/protocol/protocol.go` | JSON-RPC 信封、业务错误码（-32000~-32019）、ErrorTable（三端生成的唯一真源）、NewResult/NewError（自动附 diag）。[P1-5] |
| `kernel/pkg/protocol/codes.go` | §19 诊断码常量（E_*）与宿主/插件/内核共用方法名常量。[P1-5][P2-41] |
| `kernel/pkg/protocol/errors.json` | protocolgen 生成的方言中立错误码表（与三端生成物同源）。[P1-5] |
| `kernel/go.mod` / `kernel/go.sum` | Go 模块定义与依赖锁（gorilla/websocket v1.5.3、golang.org/x/sys v0.48.0，go 1.26.0）。[P1-6] |
| `kernel/kerneld.exe` | 预编译内核二进制（宿主启动对象，go.mod 要求 Go 1.26 构建）。 |
| `kernel/kernel/kerneld.exe` | 误入嵌套目录 `kernel/kernel/` 且**已被 git 跟踪**的重复二进制（约 10MB）。[P0-2] |
| `kernel/kerneld.exe.bak-20261013-152326` | 备份二进制垃圾（10.6MB，未跟踪也未忽略）。[P0-3] |
| `kernel/kerneld.exe.inuse-20261013-152457` | “使用中”副本垃圾（10.6MB，未跟踪也未忽略）。[P0-3] |
| `kernel/kerneld.exe~` | 临时副本（被 `*~` 规则忽略）。[P0-3] |

### 2. ui（Electron 宿主）

| 文件 | 功能一句话 |
|---|---|
| `ui/index.html` | 宿主渲染页：顶栏/侧栏/设置卡片（A~L 共 11 节）/添加插件（插件+工具 Tab）/命令 palette/插件 iframe 挂载区 + 全部内联 CSS。[P1-8][P2-33][P2-34][P2-37] |
| `ui/package.json` | 宿主 npm 清单：`build`=tsc + 拷贝 octsel.js，`start`=build 后起 electron；依赖 ws。 |
| `ui/package-lock.json` | npm 依赖锁文件。 |
| `ui/tsconfig.json` | TS 编译配置（commonjs、输出 build/、非 strict、含 DOM lib）。 |
| `ui/.npmrc` | npmmirror 镜像配置（registry/electron/electron-builder）。 |
| `ui/src/main.ts` | Electron 主进程：拉起内核读 auth、WS 连接与 RPC 桥、全局热键注册/冲突检测、托盘、overlay 全屏窗管理、剪贴板同步引擎、文件关联注册、设置子窗、退出前 proxy 优雅关闭。[P1-7][P1-9][P2-35][P2-36][P2-38][P2-51][P3-52] |
| `ui/src/renderer.ts` | 宿主渲染进程：侧栏/面板装配、插件 iframe 懒加载与 oct.hello 握手、设置各表单（生命周期/排序/依赖源/热键/资源/内部设置）、命令面板、内核事件与 tool 事件转发、文件注入重试。[P0-1][P1-9][P2-30][P2-31][P2-32][P2-78] |
| `ui/src/editor-preload.ts` | 无边框插件子窗 preload：向子页暴露 `octWin`（窗口控制/剪贴板/overlay 开关/阶段切换/最大化状态订阅）。 |
| `ui/src/protocol-errors.ts` | protocolgen 生成的错误码常量表（ECodes/EMessages/EDiagByCode，勿手改）。[P1-5] |

### 3. sdk（插件 SDK，Node 与 Python 等价契约）

| 文件 | 功能一句话 |
|---|---|
| `sdk/node/src/index.js` | Node 插件 SDK：fd1 协议接管（console.* 全量改道 stderr）、握手、行读循环、channel/gate/busy/register/run。[P1-45] |
| `sdk/node/src/protocol_errors.js` | protocolgen 生成的 Node 错误码常量（CODES/MESSAGES/DIAG_BY_CODE）。[P1-5] |
| `sdk/python/oct_sdk/__init__.py` | Python 插件 SDK：fd1 接管（含 win32 utf-8 reconfigure）、握手、读循环+worker 双线程、channel/gate/busy/register/run、ProtocolError。[P1-43][P1-45][P2-44] |
| `sdk/python/oct_sdk/models.py` | tool 侧模型加载权实现：ensure/release/acquire（§19.6 未就绪报 E_MODEL_NOT_READY）+ `install_handlers` 注册 model.ensure/release/models。[P1-46] |
| `sdk/python/oct_sdk/protocol_errors.py` | protocolgen 生成的 Python 错误码常量。[P1-5] |

### 4. tools（自检/E2E 脚本）

| 文件 | 功能一句话 |
|---|---|
| `tools/check-ui-wiring.mjs` | 静态自检：交叉比对插件页 JS 引用的元素 id 是否都存在于 index.html。[P2-46] |
| `tools/e2e-conv-all.mjs` | conversion 全格式转换矩阵 E2E（拉起内核逐个格式跑转换并断言产物）。 |
| `tools/e2e-diag.mjs` | pdf/bmp 直调底层 tool 的诊断脚本，暴露 conversion 包装隐藏的真实错误。[P1-18] |
| `tools/e2e-smoke.mjs` | 内核 E2E 冒烟：模型选择链/加载权与 handover/转换图/依赖缺口验收（先删 registry.json 重基线）。 |

### 5. 根目录脚本与配置

| 文件 | 功能一句话 |
|---|---|
| `build.ps1` | 构建脚本：编译内核（失败回退预编译 kerneld.exe）+ `npm install`，支持 `-start/-rebuild`。[P1-6] |
| `package.ps1` | 打包脚本：产出可移植目录（UAC launcher exe + Electron 运行时 + kernel/plugins/sdk/config/state/runtime/resources/bin 布局 + venv 策略）。[P1-18] |
| `start.bat` | 一键启动：fnm 初始化→按需编内核→装 UI 依赖→tsc→起 electron。[P1-6] |
| `.gitignore` | 忽略规则：构建产物/state/config/runtime/plugins 等，含内核 Go 包的否定放行注释。[P0-3] |

### 6. resources（共享资源）

| 文件 | 功能一句话 |
|---|---|
| `resources/outtools.json` | outtool 描述清单（ffmpeg/pandoc/ollama 的模型声明载体），当前 `outtools` 为空对象。 |
| `resources/theme/octsel.js` | 「搜索+选择」组合框共享实现（宿主设置页与插件 iframe 共用一份，构建时拷入 ui/build/）。 |
| `resources/shanhe12/gen_cycle.py` | 山河12 配色周期图生成脚本（资源生产工具）。 |
| `resources/icons/*`（9 png/svg）、`resources/logo/*` | 托盘/窗口/logo 图标资源（含 DragList 把手 `menu-order.svg`）。[P2-34] |

### 7. 根目录散落文件（与代码无关或来源不明）

| 文件 | 功能一句话 |
|---|---|
| `_home.html` | **与项目完全无关**的第三方 HR SaaS（薪人薪事）登录页，疑似误提交入库。[P1-16] |
| `_state.json` | 来源不明的状态 JSON（20KB），无任何代码引用。[P1-16] |
| `tmp_up.txt` | 0 字节临时文件。[P0-3] |
| `screen_full.png`、`tray_crop.png`、`tray_strip.png`、`tray_zoom.png` | 根目录散落截图（未跟踪、未忽略）。[P0-3] |
| `models/.gitkeep`、`outputs/proxy-ui-preview/*` | 模型目录占位；proxy UI 预览产物（node-picker 预览 html/css）。 |
| `.trae/documents/*.md`（15 份） | 阶段计划文档（oim 工具条/colorpicker/napi 架构/打包/UI 重写等）。[P3-83] |
| `.workbuddy-ai/memory/*.md`（4 份） | 历史工作记忆日志。[P3-83] |
| `.uv-cache/` | uv 包缓存目录（archive/wheels/simple 等）混在仓库树内。[P3-83] |
| `插件设计规范.md`、`理论应实现.v1.md`、`理想架构.md` | 设计文档三件套（规范/理论实现/理想架构，代码注释大量引用其章节号）。[P3-83] |

---

## 二、问题详情（按严重级）

### P0 阻断性（4 项）

#### P0-1 命令面板渲染系统命令即崩溃（`p.params.length` 空引用）
- 位置：`ui/src/renderer.ts:1473-1507`
- `SYS_CMDS` 条目只有 `{name, desc, kind, act}`，**没有 `params` 字段**；而 `renderPalette()` 第 1504 行对 PALETTE 中每一项执行 `p.params.length`。Ctrl+K 打开命令面板 → `refreshPalette().then(renderPalette)` 内抛 `TypeError: Cannot read properties of undefined (reading 'length')` → 面板列表始终空白，命令面板核心功能不可用。
- 修复：`(p.params || []).length`，并给 SYS_CMDS 补 `params: []`。

#### P0-2 `kernel/internal/config` 与 `kernel/internal/runtime` 两个 Go 包未入库（克隆必编译失败）
- 位置：仓库 git 状态（`git status` 显示 `?? kernel/internal/config/`、`?? kernel/internal/runtime/`）。
- `.gitignore` 第 55-60 行的注释明确记录过事故：`config/`、`runtime/` 无斜杠通配规则曾把这两个**同名 Go 包**一起忽略，导致换机/克隆后 `undefined: config.SchemaProps` 编译失败；现已补否定规则 `!kernel/internal/config/`、`!kernel/internal/runtime/`，但这两个目录**从未 `git add`**，`app.go`/`ws.go`/`manager.go` 均 import 它们——当前任何全新克隆依然无法 `go build`。
- 修复：`git add kernel/internal/config kernel/internal/runtime` 并提交。

#### P0-3 仓库卫生：10MB 级二进制垃圾与无关文件
- `kernel/kernel/kerneld.exe`：嵌套重复目录且**已被 git 跟踪**（`git ls-files` 可见），应 `git rm -r --cached kernel/kernel`。
- `kernel/kerneld.exe.bak-20261013-152326`、`kernel/kerneld.exe.inuse-20261013-152457`：各 10.6MB，未跟踪也未被 `.gitignore` 覆盖（`*.bak` 不匹配 `*.bak-20261013-*`），随时可能被误提交。
- `tmp_up.txt`（0 字节）、根目录 4 张截图（screen_full/tray_*）。
- 修复：删除或移出仓库；`.gitignore` 增加 `*.bak-*`、`*.inuse-*`、`screen_full.png`、`tray_*.png`、`tmp_up.txt`。

#### P0-4 依赖安装整链是假实现（文档宣称“真实安装”，实际 no-op）
- 位置：`kernel/internal/runtime/runtime.go:144-243`
  - `Ready()` 恒返回 `true`（第 145-147 行，“骨架版保守返回 true”）；
  - `Install()` 只回调 `"skipping (runtime skeleton)"` 后返回 nil（第 178-183 行），**不执行 uv venv/pip install**；
  - `Preview()` 返回空、`ListPackages()`/`NodePackages()` 返回空、`DepsPlan()` 的 `lockHash` 恒为空串。
- 与之冲突的是：`manager.go:192-199` 注释“§6.4 真实依赖安装（Installer.Install）”、`app.go:81` “依赖未命中时后台真实安装（含进度回调）再续启”、`ws.go` 的 `deps.install` RPC、`renderer.ts:1047-1058` 的“安装并重启”按钮与“首次/重装需数分钟”提示、`package.ps1` 头部“kernel installs them on demand at first call”。
- 后果：用户在设置页点“安装并重启”毫无效果；精简分发版“首启用例自动联网安装”不成立；`registry.json.lockHash` 永远为空使 §6 锁校验形同虚设。
- 性质：这是全项目最大的“文档↔实现”背离，属于架构级 P0。

### P1 冲突 / 不一致（12 项）

#### P1-5 错误码表不同源：`E_MODEL_NOT_READY(-32019)` 丢了
- `kernel/pkg/protocol/protocol.go:64-65` 定义了 `ErrModelNotReady = -32019` 与 `DiagModelNotReady`，`codes.go:29-30` 有诊断码，但 **`ErrorTable`（第 108-130 行）没有收录它**。
- 后果：内核 `NewError(-32019, …)` 时 `DiagOf` 返回空 → `error.data.diag` 为空；protocolgen 生成的三端产物（`ui/src/protocol-errors.ts`、`sdk/node/src/protocol_errors.js`、`sdk/python/oct_sdk/protocol_errors.py`、`errors.json`）全部没有 `E_MODEL_NOT_READY`；而 `sdk/python/oct_sdk/models.py:111` 恰好要 raise 这个 diag → Python 侧 `CODES.get("E_MODEL_NOT_READY")` 落空。
- 另：`codes.go:55` 注释声称“-32011~-32018 与 codes.go 的 E_* 一一对应”，但表中 -32018 之后还有 -32019，注释边界过期。
- 修复：ErrorTable 补 `{ErrModelNotReady, DiagModelNotReady, ErrText[ErrModelNotReady]}` 并重新 `go generate ./...`。

#### P1-6 Go 版本要求三处互相矛盾
- `kernel/go.mod:3`：`go 1.26.0`；
- `build.ps1:29`：失败提示写 “go.mod requires Go **1.27**”；
- `start.bat:27`：写 “need Go **1.21** or newer”。
- 修复：统一为 go.mod 的真实值，且建议脚本动态读 go.mod 而非硬编码。

#### P1-7 设置子窗口标题映射不完整（A~D vs A~L）
- `ui/src/main.ts:215` 的 `names` 只有 A/B/C/D 四项；`ui/src/renderer.ts:27` 与 `ui/index.html:292-302` 有 A~L 共 11 个设置节。用户打开 E~L 节（插件列表/内部设置/依赖/已装库/共享函数/外部地址/热键）时，窗口标题显示原始字母（如“设置 · L”）。
- 修复：两处 names 合并为单一来源（或从 index.html 的 `data-sec` 文案派生）。

#### P1-8 index.html 大量重复元素 id（隐藏 debug 面板成死代码）
- `ui/index.html:361,482` 两个 `id="list"`；`depPick/depDrop/btnDepRefresh/depArea/btnDeps/btnInstall/depResult/btnEnvs/btnEnvsClear/envsResult/btnRegRefresh/fnArea/btnCallFn/fnResult` 在设置卡片（306-423 行）与隐藏的 `#panel-debug`（478-514 行）中**各定义一份**。
- 后果：`getElementById` 只取第一份（设置卡片），debug 面板所有控件永不可达、永不被绑定；HTML id 唯一性被破坏，后续任何按 id 取元素的逻辑都会静默取错。
- 修复：删除 `#panel-debug` 整块（注释自称“保留 hidden 占位避免 JS 引用崩”，但 JS 引用的其实是设置卡片那份）。

#### P1-9 `RUNNING_EXT` 是内核从不产生的幽灵状态
- `ui/src/renderer.ts:78,89` 判断 `state === "RUNNING" || state === "RUNNING_EXT"`；内核 `RegState.String()`（`manager.go:44-57`）只产出 `REGISTERED/STARTING/RUNNING/IDLE/STOPPED`。
- 后果：死分支，且暗示两端曾有过另一套状态机（可能是历史上“常驻扩展”语义残留），读者无法判断哪个是真。
- 修复：删除 `RUNNING_EXT` 分支，或在内核补齐该状态。

#### P1-10 `perms.Subset` 返回无序列表
- `kernel/internal/perms/perms.go:174-187`：declared/granted/highRisk 直接由 map 迭代 append，顺序随机。宿主权限 UI（`plugin.details`/`perms.list`）每次刷新顺序都不同。
- 修复：返回前 `sort.Strings`。

#### P1-12 registry 迁移注释与实现不符（“MUST NOT 静默忽略未知字段”未实现）
- `kernel/internal/registry/registry.go:252` 注释宣称拒绝未知字段，但 `loadLocked` 用普通 `json.Unmarshal`（无 `DisallowUnknownFields`），未知字段被静默忽略。
- 修复：要么实现严格解析，要么改注释。

#### P1-13 状态机不变式被自己违反（数据竞争 + 绕过 stateLoop）
- `manager.go:62-63` 注释宣称“所有 r.state 改写必须是事件、经 stEvCh 汇入唯一 stateLoop，禁止多 goroutine 直接改写”。但：
  - `manager.go:434-438`（`depsInstallThenStart`，后台 goroutine）直接写 `r.depsPending`、`r.state = RegStopped`；
  - `manager.go:1032`（`loopSpawn` 内）写 `r.mf.Entry` 时**未持 `r.mu`**，而 `Describe/All/Lifecycle` 等读路径均持锁读 `r.mf` → `go test -race` 可复现的 data race；
  - `manager.go:1048` 在 stateLoop 内置 `RegIdle` 尚合规，但与 434-438 的裸写并存使“单 goroutine 状态机”名存实亡。
- 修复：deps 安装结果也封装为 stEvent（如 `opDepsDone/opDepsFail`）；`r.mf.Entry` 改写加锁或改为局部变量。

#### P1-14 `deps.envs` / perms 写路径缺少 installer/gate 判空（潜在 panic）
- `ws.go:716-728`（`handleDepsEnvs`）直接用 `s.installer.ManagedPython()`，而同文件其他 handler（`handleDepsGetConfig` 等）都有 `s.installer == nil` 检查；`ws.go:523,541`（`perms.authorize/revoke`）直接 `s.gate.Authorize/Revoke`，而 `handlePermsList`（499 行）有判空。
- 当前 app.go 恒注入所以不爆，但属于“同一文件内防御风格不一致”的潜在崩溃点。

#### P1-15 settingsSchema 过滤逻辑双实现，行为已漂移
- `ws.go:394-405`（`handlePluginSetSettings`）与 `app.go:121-141`（`SetSettingsWriter`）各写一遍“只保留 schema 声明键”：
  - ws 版条件 `if ok && len(allowed) > 0`（manifest 不存在或 schema 为空时**不过滤**）；
  - app 版 `config.SchemaProps(mf.SettingsSchema)` 后无条件过滤（schema 为空时把所有键滤光）。
- 同一语义两处实现且边界不同，后续改一处必漂移。修复：收敛为单一函数（`config.SchemaProps` 已存在，直接共用）。

#### P1-16 无关第三方页面入库
- `_home.html`：完整的“薪人薪事”HR SaaS 登录页（含 TDK/埋点/Sentry），与本项目零关系，却被 git 跟踪；`_state.json` 同样无人引用。修复：`git rm` 两个文件。

#### P1-18 硬编码开发机路径散落三处
- `package.ps1:69`：默认 `$OutDir = "D:\Project\HalfProject\OctPlugins"`（且目录名还是旧项目名 HalfProject）；
- `tools/e2e-diag.mjs:11-13`：`D:\LanguageTool\Anaconda\Library\bin\pandoc.exe`、`D:\LanguageTool\ffmpeg\bin\ffmpeg.exe`、`D:\Apps\LibreOffice\program`；
- `ui/index.html:313`：placeholder `D:\Project\ElectronProject\OCTplugin\.cache`（旧项目名）。
- 修复：改为参数/环境变量/相对默认值。

### P2 奇怪代码 / 坏味道（24 项）

| # | 位置 | 问题 |
|---|---|---|
| P2-20 | `ws.go:810` | 注释乱码：`不存在返回 “ 与 nil`（引号未闭合、缺字，应为“返回空 map 与 nil”）。 |
| P2-21 | `ws.go:198` | JSON 解析失败时回 `protocol.NewError(0, …)`，id 用 0 而非 `req.ID`，调用方无法配对。 |
| P2-22 | `ws.go:293-295` | `default` 分支直接 `ErrMethodNotFound`，无日志——客户端调错方法时内核侧无迹可查。 |
| P2-23 | `plugin.go:218` | Deframe 出错（stdout 污染）调用了 `rejectHandshake()`，日志打的是 `E_HANDSHAKE_REJECTED`，实际应为 `E_STDOUT_CONTAMINATED`——错误分类错误，排障时会被误导。 |
| P2-24 | `plugin.go:321` | `p.exitCode` 在 readLoop goroutine 无锁写，`ExitCode()` 有锁读——data race（`go test -race` 可测）。 |
| P2-25 | `plugin.go:436-442` | `gate.file_exists` **没有任何权限检查**（file_read/file_write 都有 Gate.Check）——任意插件可探测任意路径存在性，与最小权限模型矛盾。 |
| P2-26 | `resources/map.go:447` | `LastActive()` 恒返回 `time.Now()`——假实现；任何按它做空闲判断的逻辑将永不空闲（当前恰无人调用，属于埋雷）。 |
| P2-27 | `resources/tool.go:39-46` | NewTool 的 spawn 钩子执行 `Locate` 后**不看 `Found`/`Err`，恒返回 nil** → `Acquire` 对不存在的工具也置 `running` 成功，`E_TOOL_UNAVAILABLE` 永远不会从 Acquire 路径产出（`wellKnownPaths`/注册表定位全白做）。这是与 P0-4 并列的“假成功”缺陷。 |
| P2-28 | `resources/map.go:413-415` | 模型 Release 归零后状态置 `"unloading"` 后再无流转（既不会回 `registered` 也不会 `unregistered`），`ModelsSnapshot` 会长期展示 `unloading`；且 backend 工具引用在此处递归 Release，与 Acquire 的锁序（e.mu→backend.mu）虽暂不死锁，但模型 Backend 指向自身时会自锁。 |
| P2-29 | `manager.go:1390` | `d := base << attempt` 先移位、后判断 `attempt > 30`——检查顺序颠倒；`max_restarts` 配大时 attempt 可达数十，移位溢出静默产生 0 或负数时长。 |
| P2-30 | `manager.go:263-270` | `emitState` 用 `go fn(...)` 异步回调——状态事件（STARTING/RUNNING/STOPPED）到达宿主顺序不保证，侧栏圆点会闪错（renderer 的 `pollState` 轮询正是在给这个打补丁）。 |
| P2-31 | `contract.go:25` | `effectiveNet` 仅被 `contract_test.go` 使用，生产代码零调用——net 的实际执行（profile 级联网开关）从未落地，§23.3 只校验不执行。 |
| P2-32 | `renderer.ts:392` | `r._core` 字段内核 `plugin.list` 从不返回 → `PLUGIN_ROLE` 恒为 `'others'` → “核心常驻+其余按需”预设实际把**全部插件**设为 lazy，预设名不副实。 |
| P2-33 | `renderer.ts:1009` | `+ (eff.auto_restart ? "" : "")`——无副作用空表达式，疑似复制残留。 |
| P2-34 | `renderer.ts:1504` | 见 P0-1（`p.params.length`）。 |
| P2-35 | `renderer.ts:78,89` | 见 P1-9（RUNNING_EXT）。 |
| P2-36 | `renderer.ts:806` | `../resources/icons/menu-order.svg`：开发模式（ui/ 下加载 index.html）可解析；**打包模式**（index.html 位于 `resources/app/`）会解析到 `resources/resources/icons/…` 404——拖拽把手图标必坏。 |
| P2-37 | `index.html:63` | `font-family:var(--font)`——`--font` 从未在 `:root` 定义（第 8-18 行），死引用。 |
| P2-38 | `index.html:263` | logo `src="../resources/logo/logo128.png"`：打包模式下同样解析不到（首帧 404，运行时才被 `resBase` 覆盖，表现为图标闪跳）。 |
| P2-39 | `main.ts:874-903` | `startKernel` 中 auth 行解析失败 `reject` 后**不 kill `kernelProc`** —— 泄漏的内核进程继续占着端口与 Job，下次启动必撞。 |
| P2-40 | `main.ts:951-975` | `rpc()` 每次调用都 `ws.on("message", onMsg)` 新挂一个监听器，且**每条 WS 消息都会被所有在途监听器各 JSON.parse 一遍**——O(在途 RPC 数) 重复解析；renderer 600ms 轮询 + 120s 长超时并存时会累积了几十个监听器。 |
| P2-41 | `main.ts:1229` | `kernelProc.kill()` 在 Windows 上不会触发 Go 的 SIGTERM 处理器（`app.go:181` 只对 os.Interrupt/SIGTERM 有效），优雅关闭实际完全依赖 Job Object KILL_ON_JOB_CLOSE——与“信号退出”注释描述的不是一条路径。 |
| P2-42 | `main.ts:112,222` | 主窗/设置子窗/overlay 窗用 `nodeIntegration:true + contextIsolation:false`，而 `spawnPluginWindow`（184-194 行）用 `contextIsolation:true`——同宿主两种安全基线，前者让 `/plugin/` 下的任意页面具备 require 全权。 |
| P2-43 | `main.ts:58-62` | `tool:invoke` 用 20s 硬编码超时透传 `plugin.call`，与内核 manifest 声明的 `request_timeout_ms` 无联动。 |
| P2-44 | `plugin.go:428` | `time.AfterFunc(... cmd.Process.Kill())`：进程已退出时 Kill 返回错误被忽略，且 `cmd.Wait()` 前杀进程在部分场景下与 `process.KillTree` 双杀——功能无碍但属粗糙模式。 |
| P2-45 | `plugin.go:33` | 注释 `§：` 章节引用为空（manifest.go Kind 字段说明）。 |
| P2-46 | `perms.go:114` | 注释乱码：`plugins 为空可配置热键等`（应为“perms 为空表示仅配置热键等不涉及权限的场景”）。 |
| P2-47 | `codes.go:69` | 常量名 `MethodGateFileExit` 对应方法却是 `gate.file_exists`——名不副实（Exit vs Exists）。 |
| P2-48 | `lifecycle.go:27-38` | `DefaultLoadMode` 与一批 int 常量混在同一个 const 块，`DefaultAutoRestart`（bool）夹在其中——可编译但类型混杂，后续加常量易错。 |
| P2-49 | `lifecycle.go:61-66` | `StartupCfg.PrewarmPool/KeepAlive`：Override 可设、UI 可编辑（renderer 表单有这两项），但运行期**从不读取**——“常驻锁”“预热池”是纯装饰配置。 |
| P2-50 | `lifecycle.go:69-74` | `Heartbeat.IntervalMs` 被声明、被校验，`healthSweep` 却用固定 1s ticker——心跳间隔配置无效。 |
| P2-51 | `lifecycle.go:109-118` | `Limits.CPUPercent` 有声明、有 UI、有 Override，`attachLifecycle` 只消费 `MemBytes`——CPU 限制从未生效。 |
| P2-52 | `lifecycle.go:120-139` | `Triggers`（on_command/on_ui_open/on_file_ext/on_event）与 `HasTriggers()` 全仓库无消费方——懒启动触发条件是死声明。 |
| P2-53 | `config.go:197-209` | `ResolvePlugin` 注释说“先放作者 defaults 再用 user 覆盖”，实际循环只放 `user.Settings`（`DefaultsDecl` 根本没有 settings 字段）——注释描述了一段不存在的逻辑。 |
| P2-54 | `config.go:131-135` | `DefaultsDecl.StartPolicy/StopPolicy` 定义后无任何读取方（§9.1 的 trigger/mode 策略实际走 RecycleCfg.StopMode）——死字段。 |
| P2-55 | `registry.go:34-42` | `ResourceEntry.Locate/Launch/ResolvedPath/Endpoint` 结构齐全，但 `syncResourceRegistry`（resources.go:198-205）只写 `Kind+Verified`，注释自称“resolvedPath 由工具侧维护”而无任何实现——半成品字段。 |
| P2-56 | `layout.go:210-215` | 注释声称“失败时清理已移动的部分并保留原登记”，但 `moveTree` 中途失败直接返回，已移动的文件不回滚——注释与实现不符（搬迁是破坏性操作，此点会在用户确认后造成半成品状态）。 |
| P2-57 | `download.go:97-99` | 同一模型并发 `PullModel` 会写同一个 `.part` 文件互相踩；无 in-flight 去重；无断点续传（注释承认）。 |
| P2-58 | `sdk/python/__init__.py:63` | `home_dir()` 兜底 `dirname(dirname(__file__))` 返回的是 `sdk/python` 而非插件 home——环境变量缺失时语义错误。 |
| P2-59 | `sdk/python/__init__.py:101-111` | `_post()` 用 `q.get()` **无超时**阻塞；Node SDK（index.js:38-47）默认 30s 超时——双 SDK 契约不对称，Python 插件遇到内核不响应会永久挂死。 |
| P2-60 | `sdk/node/src/index.js` | 无 Python 侧等价物 `ProtocolError`（models.py:111 的语义化错误在 Node 侧只能退化为 `E_INTERNAL`）——双 SDK 能力不对称。 |
| P2-61 | `sdk/python/oct_sdk/models.py:151` | raise `ProtocolError("E_MODEL_UNAVAILABLE", …)`——该 diag **不在** protocol 错误码表中（只有 `E_TOOL_UNAVAILABLE`/`E_MODEL_NOT_READY`），`CODES.get` 落空后 code 变 -32603，宿主收到一个表外诊断码。 |
| P2-62 | `tools/check-ui-wiring.mjs:12` | 默认检查 `plugins/conversion`，但仓库无 `plugins/` 目录（gitignore 排除）→ 自检永远 SKIP，形同虚设。 |
| P2-63 | `watchdog.go:42-64` | `Watchdog.Run` 无调用方（manager 用 patrol+Tick）；`Status.Busy` 一旦置 true 且插件不释放 busy，`checkBusy` 每秒强制 ping 探活——行为可工作但状态机语义含混。 |
| P2-64 | `transport/ndjson.go`、`protocol.go:157`（`Marshal`）、`perms.go:167`（`IsHighRisk`）、`resources/discover.go`、`process/spawn.go`（经 resources.Spawn 间接）、`manager.go:241`（`SetTokenGenerator`） | 一批无调用方的死代码/死 API：FR-10 二次确认（IsHighRisk）从未接线。 |
| P2-65 | `renderer.ts:83-92` | `pollState` 600ms×8 轮询 `plugin.list`（每次全量列表）作为状态丢失兜底——高频全量拉取，中低效；根因是 P2-30 的事件乱序/丢失。 |
| P2-66 | `renderer.ts:228-305` | postMessage 目标一律 `"*"`，其中 `oct.hello` 握手把 **token 明文**广播给 iframe 内任意脚本；origin 检查用 `includes("127.0.0.1")` 弱匹配。token 经 postMessage 广播 + iframe 页面可被任意脚本读取，属中危信息暴露。 |
| P2-67 | `ws.go:1009-1025,1029-1072` | `serveRes`/`servePluginUI` 的目录穿越用大小写**敏感**的字符串前缀比较，而 Windows 路径大小写不敏感——`C:\…` 与 `c:\…` 变体理论上可绕过前缀校验（需 URL 编码配合，风险中低但存在）。 |
| P2-68 | `ws.go:126-155` | `/res/`、`/plugin/`、`/favicon` 均无 token 鉴权（仅 WS 需要 token）——本机任意进程可拉取插件 UI 与共享资源。 |

### P3 架构 / 工程化（6 项）

#### P3-69 “加载权在宿主”与“依赖安装”两大硬约束只有一半落地
- §四/§19.6（模型必须宿主加载）落实得很好（`model_hooks.go` + `models.py` + `gate.model_ensure` + handover 全链）；
- 但 §6（依赖隔离安装）整链是骨架（P0-4），§10.2 的 lockHash 校验恒空——同一套“理想架构”里两条同级约束完成度天差地大，分发策略（`package.ps1` 的 venv 三态）建立在不会工作的安装链上。

#### P3-70 双 SDK 契约漂移无防波堤
- Node 与 Python SDK 在超时（P2-59）、语义错误（P2-60）、run() 入参形态（Node: `run(handler{methods,main})`；Python: `run(main, args)`）上均已分叉；`protocolgen` 只同步错误码，不同步 API 形状。建议补一份跨 SDK 契约测试或统一 codegen。

#### P3-71 文档四套并行，章节号引用无校验
- 根目录三份大文档（`插件设计规范.md` 50KB、`理想架构.md` 63KB、`理论应实现.v1.md` 37KB）+ `.trae/documents/` 15 份计划 + `.workbuddy-ai/memory/` 4 份记忆；代码注释大量引用“§12.2.2/§15.4/§20.5”等章节号，但没有任何机制校验章节仍存在/仍匹配（P0-4、P2-49~52 等“文档说做了”的项即漂移产物）。

#### P3-72 仓库卫生：缓存/计划/记忆目录混入工作树
- `.uv-cache/`（uv 包缓存，含 archive/wheels/simple 上百目录）、`.trae/`、`.workbuddy-ai/` 均在仓库内且大部分未忽略；`outputs/`、`models/` 只有占位。建议 `.gitignore` 补 `.uv-cache/`，并明确 `.trae`/`.workbuddy-ai` 是否入库。

#### P3-73 UI 单文件巨型脚本 + 重复 DOM，可维护性触底
- `renderer.ts` 1539 行承载全部面板逻辑（设置 11 节 + 侧栏 + palette + 热键 + 资源编辑器 + iframe 桥），`index.html` 533 行内含重复 id 的死面板（P1-8）；`main.ts` 1246 行混装内核生命周期/热键/同步引擎/文件关联/overlay 五个关注点。任何设置项新增都要在三个文件（html 卡片、main names、renderer 表单）同步改，漏一处即 P1-7 类 bug。

#### P3-74 测试覆盖偏斜：内核单测充分，宿主/SDK/生成物零测试
- lifecycle 14 个测试文件覆盖契约/状态机/gating/handover 等，质量高；但 `ui/src/*.ts`（含已确认的 P0-1）、`sdk/*`、`tools/*`、protocolgen 产物与 ErrorTable 的一致性（P1-5）均无自动化校验。建议至少补：① protocolgen 后 diff 校验（ErrorTable ↔ 三端产物）；② renderer palette 冒烟（jsdom 即可）。

---

## 三、修复优先级建议

1. **立即（P0）**：`git add kernel/internal/config kernel/internal/runtime`（否则克隆即废）；删 `kernel/kernel/` 跟踪；清理 10MB 备份二进制；修 `renderer.ts:1504` 一处字符让命令面板复活。
2. **短期（P0-4 + P1 组）**：补齐 `runtime.Installer` 真实安装链或在 UI/文档中如实降级为“未实现”；ErrorTable 补 -32019 并重新生成三端；统一 Go 版本文案；合并设置 names 与 settings 过滤双实现；删 index.html 死面板。
3. **中期（P2 组）**：`gate.file_exists` 补权限位；tool Acquire 认 `Locate` 结果；修 P2-23/24/29/30 的竞争与错分；SDK 对齐超时与 ProtocolError；postMessage 收敛 origin。
4. **长期（P3 组）**：拆分 renderer/main 关注点；建立文档章节↔实现校验与 protocolgen 一致性 CI；明确 `.uv-cache/.trae/.workbuddy-ai` 的入库策略。

---

## 四、扫描覆盖度声明

- Go：61/61 个 `.go` 文件全部逐行阅读（含 14 个测试文件确认语义）。
- TS/JS：`ui/src/` 4/4 + `resources/theme/octsel.js` + `sdk/node/src/` 2/2 + `tools/` 4/4。
- Python：`sdk/python/oct_sdk/` 3/3。
- HTML/配置：`ui/index.html`、`ui/package.json`、`ui/tsconfig.json`、`ui/.npmrc`、`build.ps1`、`package.ps1`（头部 120 行）、`start.bat`、`.gitignore`、`resources/outtools.json`。
- Git 状态：`git status` / `git ls-files` / `git check-ignore` 核对跟踪与忽略实况。
- 未逐行读的大文件：`package.ps1` 全文 31KB（读了头部 120 行布局说明与参数）、`.trae/documents/*` 与三份设计文档（仅作存在性与引用核对，未逐行——它们不是代码）。
