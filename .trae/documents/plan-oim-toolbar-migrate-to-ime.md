# oim 工具条迁移：从宿主迁回输入法（oimd 托盘工具条）

## Context（为什么做这个改动）

之前的「oim 常驻工具条」被错误地实现成了 Electron 宿主（OctPlugins）的 `BrowserWindow`（468x48 右下角小条），由 `ui/src/main.ts` 轮询 `oim.state` 显隐。结果是：**宿主没打开，工具条就消失**——违背了输入法的定位。

**输入法的正确定位**：输入法是**独立应用**（`oimd.exe` = WeaselServer fork，由 TSF 自动拉起 + Run 键自启，登录即常驻），**不依赖 OctPlugins 宿主**；`plugins/oim`（Go TCP 桥）只是宿主 ⇄ oimd 之间的**桥接**，oim 插件 ≠ 输入法本体。

工具条应在输入法应用内。rime/weasel 已自带现成工具条形态——**系统托盘图标 + 右键菜单**（`WeaselTrayIcon` 已随 ascii_mode 切换中/英图标，`IDR_MENU_POPUP` 已有设置/同步/退出菜单）。本次改造即**复用 rime 的托盘工具条**，宿主侧删除 BrowserWindow 工具条。

**用户已确认**：
- 工具条形态 = 托盘图标 + 右键菜单（复用 rime 现成）
- 宿主未打开时，「打开设置/设备同步/文本翻译」三项点击 → 气泡提示「宿主未运行」（输入法自身功能中英/全角切换不受影响，本地生效）

## 架构（现状 → 目标）

```
现状（错误）：
  Electron 宿主 ──轮询──► oim.state ──► BrowserWindow 工具条（宿主退出即消失）

目标（正确）：
  oimd.exe（独立常驻，TSF/Run 键拉起）
    ├─ 托盘图标 + 右键菜单 = 工具条（中英/全角本地切换，不依赖宿主）
    └─ OctServer(56619) ──上行事件──► oim 插件(Go 桥) ──► 内核 ──► 宿主（设置/同步/翻译面板）
```

## 改动清单

### 一、oimd 侧（C++，`mvp_test/oim/weasel/`）—— 工具条主体

1. **`WeaselServer/resource.h`** 新增菜单项 ID：
   - `ID_WEASELTRAY_FULL_SHAPE`（40017）、`ID_WEASELTRAY_HALF_SHAPE`（40018）— 全角/半角
   - `ID_WEASELTRAY_OIM_SETTINGS`（40019）— 打开设置（宿主 oim 设置面板）
   - `ID_WEASELTRAY_OIM_SYNC`（40020）— 打开设备同步（宿主 sync 面板）
   - `ID_WEASELTRAY_OIM_TRANSLATE`（40021）— 打开文本翻译（宿主翻译新窗）
   - 中英切换复用已有 `ID_WEASELTRAY_ENABLE_ASCII`（40013）/ `ID_WEASELTRAY_DISABLE_ASCII`（40014）

> ⚠️ **勘误（2026-10-13 修正）**：本文档下面 §3 写的
> 「`MENUITEM "中文输入"` → `ID_WEASELTRAY_ENABLE_ASCII`」**是错的**，已据此踩坑
> （工具条中英切换变成 no-op）。`WeaselServerImpl::OnCommand` 与 `/ascii` CLI 的语义是
> **`ENABLE_ASCII` → `ascii_mode=true` → 西文**、`DISABLE_ASCII` → `ascii_mode=false` → 中文；
> 据此 `.rc` 标签、`WeaselTrayIcon::CustomizeMenu` 单选勾选、`OimToolbar` 的 `ITEM_EN/ITEM_ZH`
> 三处已全部对齐。详见 `plan-oim-ime-toolbar-strip.md` §11 与 `mvp_test/oim/HANDOFF.md` §五.12。

2. **`WeaselServer/WeaselServer.rc`**（`IDR_MENU_POPUP`，三语言段 CN/TW/EN 同步加）在「输入法设定」上方插入：
   - `MENUITEM "中文输入 (&Z)"` → `ID_WEASELTRAY_ENABLE_ASCII`
   - `MENUITEM "西文输入 (&E)"` → `ID_WEASELTRAY_DISABLE_ASCII`
   - `MENUITEM "全角符号 (&F)"` → `ID_WEASELTRAY_FULL_SHAPE`
   - `MENUITEM "半角符号 (&H)"` → `ID_WEASELTRAY_HALF_SHAPE`
   - `MENUITEM SEPARATOR`
   - `MENUITEM "打开设置 (&O)"` → `ID_WEASELTRAY_OIM_SETTINGS`
   - `MENUITEM "设备同步 (&S)"` → `ID_WEASELTRAY_OIM_SYNC`
   - `MENUITEM "文本翻译 (&T)"` → `ID_WEASELTRAY_OIM_TRANSLATE`

3. **`WeaselIPCServer/WeaselServerImpl.cpp`** `OnCommand` 新增分支（仿照已有 ascii 分支）：
   ```cpp
   case ID_WEASELTRAY_FULL_SHAPE:
     m_pRequestHandler->SetOption(lParam, "full_shape", true); return 0;
   case ID_WEASELTRAY_HALF_SHAPE:
     m_pRequestHandler->SetOption(lParam, "full_shape", false); return 0;
   ```
   （lParam=0 走全局/活跃会话，与 ascii 分支一致；SetOption 内部会 `_UpdateUI` 刷新托盘图标）

4. **`WeaselServer/WeaselServerApp.cpp`** `SetupMenuHandlers` 注册三个 OIM 动作（菜单回调在线程安全的 OctServer::Push 内）：
   - `ID_WEASELTRAY_OIM_SETTINGS` → 宿主开 oim 设置面板
   - `ID_WEASELTRAY_OIM_SYNC` → 宿主开设备同步面板
   - `ID_WEASELTRAY_OIM_TRANSLATE` → 宿主开翻译新窗
   - 统一逻辑：`if (oct_ && oct_->HasClients()) oct_->Push("oim.open_panel", json)` 否则 `tray_icon.ShowBalloon(L"OctPlugins 宿主未运行", ...)`（CSystemTray 自带 ShowBalloon）
   - Push 载荷：
     - settings → `{"panel":"plugin.oim"}`
     - sync → `{"panel":"plugin.sync"}`
     - translate → `{"panel":"translate"}`

5. **`WeaselServer/OctServer.{h,cpp}`** 新增 `bool HasClients() const`（`clients_` 非空判断，加锁）。
   `WeaselServerApp.h` 前向声明已含 `OctServer`，成员 `oct_` 已存在，无需新增。

6. **`WeaselServer/WeaselTrayIcon.{h,cpp}`** 菜单勾选态（可选增强，工作量小）：
   - `CustomizeMenu(HMENU)` 虚函数目前是空实现，`CSystemTray::OnTrayNotification` 在 `TrackPopupMenu` 前调用它 —— 在此按 `m_status.ascii_mode` / `m_status.full_shape`（`WeaselIPCData.h` Status 结构已有两字段）对 4 个输入态菜单项 `CheckMenuRadioItem`，实现当前状态勾选显示。

### 二、宿主侧（TS，`ui/src/`）—— 删除工具条窗口，改为接收上行事件

1. **`ui/src/main.ts`** 删除 oimBar 全部：
   - `oimBarWin / oimBarPoll / oimBarBusy` 变量、`oimBarUrl / showOimBar / hideOimBar / startOimBarPoll` 函数
   - `ipcMain.handle("oimbar:panel|translate|close")` 三个 handler
   - `app.whenReady` 里的 `startOimBarPoll()` 调用
2. **`ui/src/main.ts`** `connect()` 的 `ws.on("message")`（L906-L926）新增 oim 上行事件分支（在 overlay 路由之后、`kernel:event` 转发之前）：
   ```ts
   if (p.source === "oim" && msg.params && msg.params.type) {
     const t = msg.params.type; const d = (msg.params.data) || {};
     if (t === "oim.open_panel") {
       if (win && !win.isDestroyed()) { win.show(); win.webContents.send("ui:switch-panel", d.panel); }
     } else if (t === "oim.open_translate") {
       spawnPluginWindow(translateTextWinUrl(), { title: "文本翻译", width: 520, height: 460, frameless: true });
     }
   }
   ```
   - 面板名 `plugin.oim` / `plugin.sync` 直接复用 renderer 现有 `switchPanel`（[renderer.ts](file:///d:/Project/ElectronProject/OctPlugins/ui/src/renderer.ts#L1479-L1481) `ui:switch-panel` 监听已有，无需新增）
   - 翻译新窗 URL 复用现有 text_win 拼法（`/plugin/translate/ui/text_win.html?auth=&kport=`，即原 `oimbar:translate` 内的逻辑，抽成小函数）
3. **删除文件**：
   - `ui/src/oimbar-preload.ts`
   - `plugins/oim/ui/bar.html`
   - `ui/build/` 中对应编译产物（oimbar-preload.js 等，若存在）
4. renderer.ts 的 `ui:switch-panel` 监听**保留**（宿主主进程仍会用它）。

### 三、oim 插件侧（Go，`plugins/oim/main.go`）—— 无需改动

`readOimd` 已把 oimd 任何上行 method（如 `oim.open_panel`）识别后 `emitEvent(env.Method, data)` → 内核 `event.emit` → 宿主 ws 事件（source=`oim`）。链路已通，**零改动**。

## 验证

1. **重编 oimd**：`mvp_test/oim/weasel/build.bat`（msbuild x64 Release，HANDOFF 已确认 EXIT=0 可跑通），产物 `output/oimd.exe` 同步到 `plugins/oim/ime/oimd.exe`（若有）。
2. **重启 oimd**（杀旧进程后由 Run 键/TSF 重拉），观察系统托盘出现输入法图标。
3. **宿主关闭状态**（关键验收）：不启动 OctPlugins 宿主，托盘图标应**仍然存在**；右键菜单：
   - 点「中文/西文」「全角/半角」→ 输入法状态本地切换（图标变化），**不依赖宿主**；
   - 点「打开设置/设备同步/文本翻译」→ 气泡提示「OctPlugins 宿主未运行」。
4. **宿主启动状态**：启动宿主后重复上述 → 前两项依旧本地生效；后三项打开宿主对应面板（oim 设置 / 设备同步）与翻译新窗。
5. **回归**：宿主不再创建 468x48 工具条窗口（无 `[oimbar]` 日志）；设备同步链路不受影响。

## 关键文件速查

| 文件 | 动作 |
|---|---|
| `mvp_test/oim/weasel/WeaselServer/resource.h` | 加 5 个菜单 ID |
| `mvp_test/oim/weasel/WeaselServer/WeaselServer.rc` | IDR_MENU_POPUP 加 7 项 ×3 语言 |
| `mvp_test/oim/weasel/WeaselIPCServer/WeaselServerImpl.cpp` | OnCommand 加 full_shape 分支 |
| `mvp_test/oim/weasel/WeaselServer/WeaselServerApp.cpp` | SetupMenuHandlers 注册 3 个 OIM 动作 |
| `mvp_test/oim/weasel/WeaselServer/OctServer.{h,cpp}` | 加 HasClients() |
| `mvp_test/oim/weasel/WeaselServer/WeaselTrayIcon.{h,cpp}` | CustomizeMenu 勾选态（可选） |
| `ui/src/main.ts` | 删 oimBar 全部 + 加 oim 上行事件分支 |
| `ui/src/oimbar-preload.ts`、`plugins/oim/ui/bar.html` | 删除 |
