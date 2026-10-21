# 设备同步改名 + oim 常驻工具条 实施计划

## Context（背景）

剪贴板同步已上线（宿主引擎 + sync 插件 + 公网服务器）。用户两点新需求：

1. **改名**：「剪贴板同步」→「设备同步」，语义更贴合（剪贴板 + 文件 + 多设备）。
2. **oim 常驻工具条**：像输入法状态条一样的小横条，跟随输入法通道启用/停用显示，提供入口：打开设置（oim 主面板）、切换中英文、切换中英文符号、打开 OCT 设备同步（sync 面板）、打开新窗口文本翻译（translate）。

用户已确认三个决策：
- 常驻框显示时机：**跟随 oim 状态**（oim 启用即显示，停用即隐藏）
- 「打开设置」：**oim 主面板**（切换宿主侧栏到 plugin.oim）
- 「新窗口文本翻译」：**复用结果窗**（translate 的 result_win.html 布局形态，但做文本输入翻译窗）

## 关键机制（已探明）

- **oim 后端**：`plugin.call("oim.option.set", {name:"ascii_mode"|"full_shape", value:bool})` 切换中英/全角；`plugin.call("oim.state")` 返回 `{enabled, ascii_mode, full_shape, schema_id}`（见 plugins/oim/main.go L236-241 与 ui/app.js L104-105）。
- **宿主开窗**：`spawnPluginWindow(full, {frameless,width,height})` 通用开窗；`openHotkeyWindow` 同款 URL 拼接 `http://127.0.0.1:<port>/plugin/<pid>/<page>?auth=..&kport=..` 让子页自连内核 WS（main.ts L733-743）。
- **宿主 → renderer 切面板**：`switchPanel(name)` 在 renderer 本地（renderer.ts L1478），目前无主进程触发入口，需新增 `ipcRenderer.on("ui:switch-panel")`。
- **插件名显示**：侧栏按钮取 `plugin.list` 的 `name`（renderer.ts L138-197），改 manifest `name` 即全局生效。
- **子窗窗口控制**：editor-preload 暴露 `octWin`（min/max/close）供 frameless 自绘标题栏；新 preload 仿此。

## 实施步骤

### 1. 改名「剪贴板同步」→「设备同步」
- `plugins/sync/manifest.json`：`name` → `"设备同步"`
- `plugins/sync/ui/index.html`：`<title>` 与顶栏 `h1`「剪贴板同步」→「设备同步」
- grep 全仓库残留「剪贴板同步」（含 app.js 状态文案），全部替换

### 2. 宿主 main.ts：oim 常驻工具条窗口管理（新增，位于热键/overlay 区附近）
- `oimBarWin` 单例；`createOimBar()`：`new BrowserWindow`（约 360x44，frameless、alwaysOnTop、skipTaskbar、focusable:false、无 shadow，`webPreferences.preload = oimbar-preload.js`），加载 `http://127.0.0.1:<kport>/plugin/oim/ui/bar.html?auth=..&kport=..`（复用 kernelAuth 拼接）
- `setInterval` 1s 轮询 `rpc("plugin.call",{pluginId:"oim",method:"state"})`：成功且 `enabled===true` → show（未创建则创建）；失败或 disabled → hide。轮询失败视为未启用（oim 未加载）
- IPC：
  - `ipcMain.handle("oimbar:panel", (e,name))` → 主窗口 `win.show()` + `win.webContents.send("ui:switch-panel", name)`
  - `ipcMain.handle("oimbar:translate")` → 仿 `openHotkeyWindow` 打开 `translate/ui/text_win.html`（新窗，见步骤 4）
  - `ipcMain.handle("oimbar:close")` → hide 工具条
  - 复用 `win:ctrl`（min/close）供工具条自绘按钮

### 3. 新建 `ui/src/oimbar-preload.ts`
仿 editor-preload，`contextBridge.exposeInMainWorld("octBar", {...})`：
- `switchPanel(name)` → `ipcRenderer.invoke("oimbar:panel", name)`
- `openTranslate()` → `ipcRenderer.invoke("oimbar:translate")`
- `close()` → `ipcRenderer.invoke("oimbar:close")`
- `min()/close()` → `ipcRenderer.send("win:ctrl", ...)`

### 4. 新建 `plugins/translate/ui/text_win.html`（文本翻译新窗）
复用 result_win 布局（自绘标题栏 + 工具条 + 内容区 + 状态栏），白底极简暗金（sync/oim tokens），内容为：
- 多行输入框 + 方向选择（auto/zh2en/en2zh 下拉）+ 「翻译」按钮 + 「复制」按钮
- 结果区显示 `r.text` 与 meta（source/target/provider）
- 页面自连内核 WS（`?auth=&kport=`），`plugin.call("translate.translate",{text, direction, engine:""})`
- 窗口控制走 `octWin` preload（同 editor-preload 注入方式）

### 5. 新建 `plugins/oim/ui/bar.html` + `bar.js`（常驻工具条）
- 小横条：左状态点 + 中英/全角 chip + 右侧按钮：设置、设备同步、翻译、关闭
- 白底极简暗金（oim tokens）
- bar.js：自连内核 WS（auth/kport）→ 每 1s 轮询 `oim.state` 刷新中英/全角显示；按钮动作：
  - 设置 → `octBar.switchPanel("plugin.oim")`
  - 设备同步 → `octBar.switchPanel("plugin.sync")`
  - 翻译 → `octBar.openTranslate()`
  - 中英/全角切换 → `plugin.call("oim.option.set",{name,value:!cur})` 后立即刷新
  - 关闭 → `octBar.close()`

### 6. renderer.ts：主进程触发的面板切换
在 `switchPanel` 定义后新增：
```ts
ipcRenderer.on("ui:switch-panel", (_e: any, name?: string) => { if (name) switchPanel(name); });
```

### 7. 构建
- `ui` 下 `npm run build`（编译 main.ts + oimbar-preload.ts）
- 重启宿主（杀 electron → npm start）

## 验证（端到端）

1. 侧栏 sync 按钮显示「设备同步」；sync 面板标题同步改名
2. oim 面板点「启用」→ 常驻工具条出现（1s 内）；点「停用」→ 工具条消失
3. 工具条「中英」按钮 → oim 面板中英 chip 变化；「全角」按钮 → 全角 chip 变化
4. 工具条「设置」→ 宿主切到 oim 主面板；「设备同步」→ 切到 sync 面板
5. 工具条「翻译」→ 新窗口打开，输入英文 → 翻译 → 显示中文译文；复制按钮可用
6. 工具条「关闭」→ 隐藏；oim 重新启用 → 再次出现

## 涉及文件

- 改：`plugins/sync/manifest.json`、`plugins/sync/ui/index.html`（+app.js 文案）
- 改：`ui/src/main.ts`（工具条窗口管理 + IPC + 轮询）、`ui/src/renderer.ts`（ui:switch-panel）
- 新：`ui/src/oimbar-preload.ts`、`plugins/oim/ui/bar.html`、`plugins/oim/ui/bar.js`、`plugins/translate/ui/text_win.html`
