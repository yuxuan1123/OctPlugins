# 内核源码重建对齐《理想架构.md》

## Context（为什么做这件事）

`d:\Project\ElectronProject\OctPlugins\kernel` 目前是参考项目 `OCTplugin` 的**原样拷贝**，结构是旧的 `internal/{ipc,supervisor,perms,protocol,deps}`，与《理想架构.md》§4 声明的目录不符；更重要的是**源码在本机无法编译**：

1. `go.mod` 要求 `go 1.27.1`，本机 Go 仅 `1.26.5`；
2. 缺失 `internal/deps` 包 —— 参考项目自己的 `.gitignore` 的 `deps/` 规则误删了 `kernel/internal/deps/`，该包从未入库，但在 `cmd/kerneld/main.go` 与 `internal/ipc/ws.go` 被大量引用（uv 隔离 venv、`RequirementsSatisfied` 依赖探测、`NodeInterp`、镜像源/缓存目录配置，以及 `deps.*` 全套 WS 方法）。

上一阶段用预编译 `kerneld.exe` 绕过，但这不是"对齐 md"。用户要求**从源码完整重建内核对齐 md**。

### 已确认的决策
- **Go 版本**：`go.mod` 降到 `go 1.26`，直接用本机 `go build`，不再依赖预编译 exe。
- **对齐深度**：按 md §4 重建目录结构，同时**保留现有全套 WS 方法**（`plugin.* / perms.* / deps.* / command.list / plugin.getLifecycle / plugin.updateSettings` 等），保证现有宿主 UI（`ui/`）完整可用、不白屏。
- **SDK**：一并搭 `sdk/python/oct_sdk` 与 `sdk/node/src` 最小骨架（md §16.5 / §22）。

## 目标

`OctPlugins/kernel` 变为一个可由本机 `go 1.26` 直接 `go build` 通过的 Go 模块，目录对齐 md §4，对外 WS 契约与现有宿主完全兼容，内置 Python + Node SDK 骨架。

## 目录结构（对齐 md §4，module 名沿用 `github.com/octplugin/kernel` 以最小化 import 改动）

```
kernel/
├── go.mod                      # module github.com/octplugin/kernel; go 1.26; require gorilla/websocket v1.5.3
├── cmd/kerneld/main.go         # 入口：装配、auth 输出、阻塞（复用现有 main.go，改 import 路径）
├── internal/
│   ├── app/                    # (md) 装配粘合：把 main.go 里 newToken/装配下沉，可选；一期放 cmd 即可
│   ├── config/                 # (md) 三层配置读取/合并/迁移——本期仅承载 user-settings/registry 读取骨架
│   ├── lifecycle/              # 由 supervisor 迁入：Manager、Plugin、manifest、生命周期策略/退避/心跳
│   ├── runtime/                # (md) 代码依赖：由缺失的 deps.Installer 重建（uv/venv/node + lock 概念占位）
│   ├── resources/              # (md §14) ResourceMap 骨架（kind:tool 一期，model P2 占位）
│   ├── transport/              # (md) NDJSON Deframer/帧校验——从宿主/插件 stdio 读取侧迁入
│   ├── process/                # (md §13) spawn/KillTree：_windows.go(Job Object) / _unix.go(进程组)
│   ├── ipcserver/              # 由 ipc 迁入：WS 对宿主服务 + 静态资源(/res /plugin /favicon) + 事件广播
│   ├── perms/                  # 保留：Gate 权限校验
│   ├── watchdog/               # (md §13.3) 心跳/空闲计时调度器骨架
│   └── registry/               # (md §8) state/registry.json 唯一写入方骨架 + 原子写
├── pkg/protocol/               # 由 internal/protocol 迁入：RPC 信封 + 数字码/E_* 诊断码 + 方法名常量
├── sdk/python/oct_sdk/         # Python SDK 最小骨架（§11.2 接管stdout + §11.4 握手 + §5.2 路径 + ping）
├── sdk/node/src/               # Node SDK 最小骨架
└── (runtime 数据目录出厂由程序创建)
```

## 实施步骤

### 1. go.mod 降版并建目录骨架
- 编辑 `kernel/go.mod`：`go 1.27.1` → `go 1.26`，保留 module 名与 `require github.com/gorilla/websocket v1.5.3`。
- 建 `internal/{app,config,runtime,resources,transport,process,ipcserver,watchdog,registry}`、`pkg/protocol`、`sdk/python/oct_sdk`、`sdk/node/src` 空目录并放置迁入/新建文件。

### 2. 迁移现有包（重命名归位 + 改 import）
- `internal/protocol` → `pkg/protocol`：迁入 `protocol.go`（RPC 信封 `Request/Response/RPCError/Notification`）。同时按 md §19 增加方法名常量与 `E_*` 诊断码常量表（新增 `codes.go`）。
- `internal/ipc` → `internal/ipcserver`：迁入 `ws.go`（Server、WS 分发、`/res` `/plugin` `/favicon` 静态、事件广播 `event` 通知）。改 import 路径。
- `internal/supervisor` → `internal/lifecycle`：迁入 `manager.go`、`plugin.go`、`manifest.go`、`lifecycle.go`（`RegState`、退避重启、心跳、空闲回收）。改 import 路径。
- `cmd/kerneld/main.go`：改写 import，路径常量从基于 `store/` 微调（保持与宿主兼容）。`newToken` 保留。

### 3. 重建缺失的 `internal/runtime`（原 `internal/deps`）—— 最大工作项
从零补写依赖安装器，满足 `main.go` 与 `ipcserver/ws.go` 用到的接口，使 `deps.*` WS 方法保持可用：
- `Installer` 结构：`New(root, uvBin)`、`UseConfigFile(path)`/`LoadConfig()`（读 `store/deps.json` 镜像源/缓存目录）、`SetIndex([]string)`、`SetCacheDir(string)`。
- `VenvPython(mf Manifest) string`：返回插件隔离 `.venv` 的 Python 解释器路径（md §6.5 Python Provider）。
- `RequirementsSatisfied(mf Manifest) bool`：探活式判断依赖是否就绪（若不可靠则先返回 true + 注释说明，避免阻塞加载）。
- `NodeInterp(mf Manifest) (string, bool)`：Node 插件解释器（PATH 查找 `node`）。
- 支持 `ws.go` 中 `deps.preview / deps.install / deps.getConfig / deps.setConfig / deps.envs` 的 handler 类型（含 `deps.Pkg` 类型）。
- 在 io 出错路径合理容错：若 `uv` 缺失，`VenvPython` 回退 `python`、依赖就绪判定保守放行，先保证**能编译、能加载插件、UI 能用**；uv 真正安装作为后续增强。

### 4. 新增 md §13 进程树回收（`internal/process`）
- `spawn.go`：`Spawn(spec, token, home, data)`，注入 `OCT_CHANNEL_TOKEN / OCT_PLUGIN_HOME / OCT_PLUGIN_DATA / PYTHONUNBUFFERED=1`，清除污染型环境变量。
- `kill_windows.go`（`//go:build windows`）：Job Object —— `CREATE_SUSPENDED → AssignProcessToJobObject → ResumeThread`，`KILL_ON_JOB_CLOSE`。
- `kill_unix.go`（`//go:build darwin || linux`）：进程组 `Setpgid` + `KillTree` 负 pid。

### 5. 新增 md §8 `internal/registry` 与 §9/§5 `config` 骨架
- `internal/registry`：`state/registry.json` 读写封装 + **原子写**（临时文件 + rename），持有插件/资源登记结构（一期最小字段）。
- `internal/config`：`user-settings.json` / `manifest` 读取与回落（mv.md §5.3 冲突消解占位）；`DisallowUnknownFields` 解析。
- 目录占位：确保 `state/registry.json`、`config/user-settings.json` 首启自动建校区不报错；保留与宿主兼容的 `store/`。

### 6. runetime 资源骨架（`internal/resources`）
- `ResourceMap` 最小实现：`Entry{ID,Kind,RefCount,Handle,...}`、`Acquire/Release`、引用计数。一期只 `kind:tool`（bin 定位/启动/停止），model 仅占位接口（md §14 P2）。

### 7. SDK 最小骨架
- `sdk/python/oct_sdk/__init__.py`：
  - 首行 `sys.stdout = sys.stderr`（§11.2 接管 stdout）；
  - win32 下 `sys.stdin.reconfigure(encoding="utf-8")` / `sys.stdout.reconfigure(encoding="utf-8")`（§11.2.1，不依赖 `PYTHONUTF8`）；
  - 读取 `OCT_CHANNEL_TOKEN`，发送 `$/handshake` 帧（§11.4）；
  - `home_dir()/data_dir()/cache_dir()` 路径访问器（§5.2）；
  - 独立线程/协程应答 `ping`（§13.3）；`busy()` 上下文管理器占位（§10.5）。
- `sdk/node/src/index.ts`：`console.log` 重定向 stderr + 握手 + ping 占位（最小 JS，避免 node 类型编译负担，先可运行）。

### 8. 移动后清理与编译
- 全局替换 import 前缀：`internal/protocol`→`pkg/protocol`、`internal/ipc`→`internal/ipcserver`、`internal/supervisor`→`internal/lifecycle`、`internal/deps`→`internal/runtime`。
- `go mod tidy` 收无其他依赖；`go build ./...` 到通过，产出 `kerneld.exe`。

## 验证（端到端）
1. `cd kernel && go build -o kerneld.exe ./cmd/kerneld` —— 本机 1.26 直接编译通过，无 `internal/deps` 等缺失包报错。
2. 运行 `.\kernel\kerneld.exe`，stdout 首行输出 `{"auth":{"port":…,"token":…}}`，无 panic。
3. `cd ui && npm start`：宿主连接内核、侧栏渲染 `home`/`demo_web`，状态点正确，iframe 完成 `oct.hello` 握手。
4. 设置面板关键交互（`plugin.getLifecycle`、`perms.*`、`deps.getConfig`）返回正常、无 method-not-found。
5. 退出宿主后无残留 `kerneld.exe` / 孙子进程（Job Object 生效）。
6. SDK：临时起一个最小 Python 插件，验证 `$/handshake` token 校验通过并能应答 `ping`（可选，依赖 uv）。

## 不在本期范围
- md §15 能力层 / 模型资源（P2）：resou secondary 仅占位。
- md §6 完整 lockHash/后台安装链：补 `internal/runtime` 先保证接口与加载，完整 uv pip compile 安装流列为后续。
- UI 侧 `state/`/`config/` 与宿主 `store/` 的目录归档统一：原样保留宿主依赖的 `store/`，避免弄挂现有 UI。
- 迁移 `ui/index.html`（100KB 单文件）—— 维持原样，仅验证内核契约匹配。