# OIM 插件定位重写：纯 RPC 编排 UI + 快捷键配置 + registry 基线修复

## Context（背景）

用户在宿主里看到 oim 插件卡片报错：

- **状态区**：`状态加载失败：plugin process not running`（E_PLUGIN_DOWN）
- **最近上屏**：`加载中…` 一直不加载

用户明确插件应有定位：**「连接 Oct 的通道」**——接受传入剪贴板、提供服务器接入口、修改输入快捷键、修改中英文切换快捷键（oim 只负责改配置文件）、可以加 UI。当前 UI 完全不是这个作用。

### 根因（已实测确认）

1. **spawn 失败**：`state\registry.json` 里 oim 登记的 `manifestHash` 是 `5f4dfa61…`，但磁盘 `plugins\oim\manifest.json` 当前 sha256 是 `b19d7e7c…`（manifest 后来加了 phrase/clipboard function 声明）。内核 `tamperCheck`（[manager.go](file:///d:/Project/ElectronProject/OctPlugins/kernel/internal/lifecycle/manager.go#L665-L686)）在 spawn 前比对登记基线，不匹配拒绝启动；`syncRegistryEntry`（同文件 L619-647）对已有条目保留旧 hash，重启内核也不刷新。内核无 `plugin.update/rescan` 刷新入口。
2. **UI 是旧架构残留**：[app.js](file:///d:/Project/ElectronProject/OctPlugins/plugins/oim/ui/app.js) 还在走 `oim.node` + `napi_rs_tool` + overlay 的已废弃「注入式」路线（activate 按钮 = load oim.node），与新的 oimd TCP 桥（Go 插件 → 127.0.0.1:56619）完全脱节。`translate`/`conversion` 插件已有纯 RPC 模板可复刻。

### 用户已确认的设计决策

- UI 功能区：**输入方案切换、剪贴板管理、快捷键配置、状态 + 通道开关**（去掉原「激活」按钮）
- 快捷键配置交互：**完整键位表编辑，点击按钮弹出新窗口编辑**
- 去掉「激活」按钮 → 状态区改为展示 + 通道开关（调 `oim.toggle`）

## 实施方案

### 1. registry 基线修复（先修 spawn，能跑起来）

做完 manifest.json 的所有改动后（本次会加 keybind 声明），统一刷新基线：

1. 用 `Get-FileHash` 重算 `plugins\oim\manifest.json` 当前 sha256（小写）。
2. **停掉宿主**（避免内核运行中重写 registry.json）。
3. 编辑 `state\registry.json` 里 `oim.manifestHash` 为最新哈希（保留 version/path/depsState/dataDir）。
4. 重启宿主 → oim 插件可 spawn，UI 状态区不再报 `plugin process not running`。

### 2. UI 重写（`plugins\oim\ui\index.html` + `app.js`）

**完全删除** `oim.node` / `OIM_NODE` / `napi_rs_tool` / `toolInvoke` / overlay / `oct.overlay` 相关代码，改为与 `plugins\translate\ui\src\app.js` 同构的纯 RPC 桥：

- `oct.hello` 收 `{port,token}` → WS 连接内核 → `kernel.hello` 鉴权
- `callBackend(method, params)` = `rpc("plugin.call", {pluginId:"oim", method, params})`（沿用现有 main.go 的 method 命名：`state/toggle/schema.list/schema.select/clipboard.get/clipboard.set/keybind.list/keybind.set/history.*`）
- `oct.tool.subscribe` 不再需要（无 tool 桥）；内核 `event` 消息（`oim.commit/context/status/clipboard` 上行）转发为自定义事件供页面展示
- UI 元素样式沿用现有暗金白底主题（`--gold`/`--gold-deep`/`--bg`），与宿主白底暗金风格一致

**页面分区**：

| 分区 | 内容 | 后端调用 |
|---|---|---|
| 状态 + 通道开关 | 当前方案 / 中英文状态 / enabled 开关 | `state`（含 status+history）、`toggle {enabled}` |
| 输入方案切换 | 下拉选择方案 | `schema.list` / `schema.select` |
| 剪贴板管理 | 显示当前剪贴板文本 + 「写入」「读取」+ 上行通知列表 | `clipboard.get` / `clipboard.set`；监听 `oim.clipboard` 上行事件 |
| 快捷键配置 | 「编辑快捷键」按钮 → 弹新窗口 | `keybind.list` / `keybind.set`（新窗口内编辑） |
| 最近上屏 | 历史 chip 列表 + 清空 | `state`（history）/ `history.clear`；监听 `oim.commit` 实时追加 |

### 3. 快捷键配置功能（oimd 侧实现，Go 转发）

**新增 oimd 方法**（[OctServer.cpp](file:///d:/Project/ElectronProject/OctPlugins/mvp_test/oim/weasel/WeaselServer/OctServer.cpp)，纯文件读写 + 热加载，不碰引擎按键逻辑——符合「oim 只修改文件」）：

- `oim.keybind.list` → 读取 `%AppData%\Rime\build\luna_pinyin_simp.schema.yaml`（编译产物，已含 `key_binder/bindings` 生效列表与 `switches`）→ 返回 `{bindings:[{when,accept,send|select|toggle}], toggles:[...]}`。读不到 build 文件时回落 `output\data\key_bindings.yaml` + `luna_pinyin_simp.schema.yaml`。
- `oim.keybind.set {bindings:[...], toggles:[...]}` → 写 `%AppData%\Rime\default.custom.yaml` 的 `patch: key_binder/bindings`（输入快捷键整表替换）+ 方案级 custom（`luna_pinyin_simp.custom.yaml` 的 `patch: "switches/@0/hotkeys"`，中英文切换热键）→ 原子写盘 → 触发热加载（复用 `RequestPhraseReload` 同款 cleanup_all_sessions 流程）。

> 说明：完整键位表编辑 = 前端把生效列表整表展示，保存时整体写回 patch。Rime 官方机制支持 custom.yaml patch，是标准做法，不重复造轮子。

**Go 插件**（[main.go](file:///d:/Project/ElectronProject/OctPlugins/plugins/oim/main.go)）新增转发：

- `keybind.list` → `forward("oim.keybind.list", nil)`
- `keybind.set` → `forward("oim.keybind.set", {bindings, toggles})`

### 4. 快捷键编辑新窗口

新增 `plugins\oim\ui\keybind_edit.html` + 复用同目录桥代码：

- 主页面「编辑快捷键」按钮 → `postMessage({type:"oct.openPluginWindow", path: location 同目录 keybind_edit.html, title:"快捷键配置", width/height})`（宿主 renderer.ts L230-238 已支持）
- 新窗口按 translate `sub_win.html` 模式：从 URL 查询参数 `auth`/`kport` 直连内核 WS 鉴权，不再走 iframe postMessage
- 窗口内容：键位表可编辑表格（when / accept / 动作），保存调 `keybind.set`，保存后提示已热加载

### 5. manifest.json 修正

- 修正 `oim.phrase.list` 描述：`custom_phrase.dict.yaml` → `custom_phrase.userdb.txt`
- 新增 function 声明：`oim.keybind.list` / `oim.keybind.set`
- （manifest 改动后必须重做 §1 的 registry 基线刷新）

## 涉及文件

- `state\registry.json`：更新 oim manifestHash（§1，最后一个改，等 manifest 定稿）
- `plugins\oim\ui\index.html`、`plugins\oim\ui\app.js`：纯 RPC 重写（§2）
- `plugins\oim\ui\keybind_edit.html`：快捷键编辑新窗口（§4）
- `plugins\oim\main.go`：新增 keybind.list/set 转发（§3）
- `plugins\oim\manifest.json`：keybind 声明 + phrase 描述修正（§5）
- `mvp_test\oim\weasel\WeaselServer\OctServer.cpp` / `OctServer.h`：`oim.keybind.list/set`（§3）
- 文档 `mvp_test\oim\oim.md`、`.trae\documents\oim-weasel-ui-horizontal-and-octchannel.md`：回写快捷键方法与 registry 基线注意事项

## 验证

1. **spawn 修复**：重启宿主后 oim 插件卡片状态区不再报 `plugin process not running`；手动 TCP `{"method":"oim.ping"}` 到 56619 正常。
2. **状态 + 通道开关**：UI 显示当前方案/中英文状态；拨开关后 `oim.status` 的 `enabled` 翻转，`ProcessKeyEvent` 直通（对照 RimeWithWeasel 既有行为）。
3. **方案切换**：下拉切换 schema，`oim.schema.select` 生效。
4. **剪贴板**：UI「写入」后系统粘贴出文本；复制文本后 1s 内收到 `oim.clipboard` 上行并展示。
5. **快捷键**：新窗口打开 → `keybind.list` 填充当前键位表 → 修改中英文切换热键保存 → 新建会话输入验证新热键生效（若 cleanup_all_sessions 不足以让 schema patch 生效，改用 rime 官方 `RimeDeploySchema`/维护流程，实测为准并回写文档）。
6. **最近上屏**：`state` 的 history 渲染 + `oim.commit` 上行实时追加。
7. **回归**：`phrase.list/set/remove`、`option.set`、`oim.status` 均正常。

## 明确不做

- 不保留任何 `oim.node` / napi_rs_tool / overlay 代码（已废弃注入式路线）。
- 不做云端同步（归 OctPlugins 主体）。
- 不做输入法引擎按键逻辑改动（oim 只改配置文件）。
