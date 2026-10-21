# OIM 输入法工具横条（随输入法出现的工具条）· 需求说明

> 状态：**已实现并通过机制级验证（2026-10-13）**，见文末「实现结果」。
> 关联文档：`plan-oim-toolbar-and-rename.md`（初版宿主工具条，已废弃）、`plan-oim-toolbar-migrate-to-ime.md`（迁入 IME 的托盘方案，已实施）、`mvp_test/oim/HANDOFF.md`（输入法本体状态）。

---

## 0. 需求原文（保持原意，禁止在此之上扩大范围）

> 当前功能已基本可用，但缺少预期中的"输入法工具条"。
> 需要实现一个**随输入法状态出现和消失的工具横条**，**位置和触发时机应与输入法界面保持一致**，并**作为输入法交互的一部分**展示。

用户补充（配图为 oimd 托盘右键菜单）：

> "就是这些应该做工具横条，**也是随输入法出现的**。"

配图菜单项（即横条要提供的全部能力）：

```
中文输入 (Z)     西文输入 (E)          ← 互斥态
全角符号 (F)     半角符号 (H)          ← 互斥态
────────────────────────
打开设置 (O)     设备同步 (S)     文本翻译 (T)   ← 动作项
```

---

## 1. 背景与前置事实（实现前必读）

| # | 事实 | 影响 |
|---|---|---|
| F1 | oimd（= WeaselServer fork）常驻，**托盘图标 + 右键菜单已提供上述 7 项**：`WeaselServer.rc` 的 `IDR_MENU_POPUP`，命令 ID = 40013/40014（中英）、40017/40018（全/半角）、40019/40020/40021（设置/同步/翻译）；中英文案见 `.rc` L157-164 | 横条**不是新功能，是新入口**：同一套能力换一种可视形态 |
| F2 | **历史教训（勿重犯）**：工具条曾以 Electron 宿主 `BrowserWindow`（`bar.html` 468×48）+ 轮询 `oim.state` 实现 → 宿主一关工具条即消失，违背"输入法独立常驻"定位，已在 `plan-oim-toolbar-migrate-to-ime.md` 中删除（`ui/src/oimbar-preload.ts`、`plugins/oim/ui/bar.html` 已删） | **本次禁止再用宿主窗口路线**；横条必须活在 oimd 内 |
| F3 | **现成钩子（2026-10-13 已完成）**：oimd 已有"输入法激活态"信号——`ServerImpl::OnStartSession/OnEndSession` 维护 `m_activeSessions`，0↔1 跃迁时 `PostMessage(WM_WEASEL_SESSION_ACTIVE)` → 消息线程回调 `Server::SetSessionActiveCallback(bool)`；`WeaselTrayIcon::SetImeActive()` 已消费该信号实现"托盘随输入法显隐" | 横条显隐**直接复用此信号**，零新增机制 |
| F4 | **参考窗口样式（同进程候选窗）**：`WeaselUI/WeaselPanel.cpp` L338 用 `WS_EX_TOOLWINDOW \| WS_EX_TOPMOST \| WS_EX_NOACTIVATE \| WS_EX_NOREDIRECTIONBITMAP` + `WS_POPUP \| WS_CLIPSIBLINGS` | 横条照抄这套样式，才能不抢焦点/不进 Alt-Tab/不打断输入 |
| F5 | 既有宿主链路：3 个动作经 `OctServer::Push("oim.open_panel"\|"oim.open_translate")` → oim 插件 `event.emit` → 宿主 `main.ts` 的 oim 分支；宿主离线时弹气泡"宿主未运行，请先启动 OctPlugins 桌面端"（`WeaselServerApp.cpp` `SetupMenuHandlers`） | 横条动作**复用同一条链路**，不新增协议 |

---

## 2. 目标（一句话）

在 **oimd 进程内**新增一个**悬浮横向工具条**，其**显隐时机与输入法激活态严格一致**，提供与现托盘菜单**等价**的 7 项能力，且**完全不依赖 OctPlugins 宿主**。

---

## 3. 关键设计决策（已定，附理由；如需推翻请走 §9）

- **D1 渲染宿主 = `oimd.exe` 原生窗口**（非 Electron 宿主）。理由：满足"随输入法出现" + 输入法独立常驻定位（F2）+ 复用激活态钩子（F3）。
- **D2 形态 = 悬浮横条窗口**，贴屏幕边缘、可拖拽、位置持久化。**不做** TSF 语言栏项（语言栏在系统任务栏带内，不是"工具横条"，且不随输入法状态显隐）。
- **D3 显隐 = 复用 `WM_WEASEL_SESSION_ACTIVE`**：首个 TSF 会话 → 显示；会话归零（切到其他输入法 / 全部应用失焦）→ 隐藏。
- **D4 与托盘菜单并存**（需求原话"也是"）：托盘菜单**保留**；横条是等价可视入口。两者**必须调用同一套命令 ID 与同一套 handler**，禁止复制业务逻辑。
- **D5 动作路由沿用既有链路**（F5），宿主离线降级为气泡提示，与托盘行为一字不差。

---

## 4. 功能需求（FR）

**FR-1 显隐时机（核心）**
- 输入法被激活（任一应用切入本输入法，出现首个 TSF 会话）→ 横条**出现**。
- 全部会话结束（用户切到其他输入法 / 切走）→ 横条**消失**。
- 与托盘图标**同一时机**（同一回调内驱动），视觉上"一起出现、一起消失"。
- 维护态（deploy/`m_disabled`）不显示。

**FR-2 形态与尺寸**
- 横向条，无标题栏，紧凑（100% DPI 建议：高 32–40px；按钮 28–32px；间距 4px；内边距 6px；圆角/描边随主题）。
- 分组布局：`[中文│西文] [全角│半角] │ [设置] [同步] [翻译]`。
- 互斥态用**高亮**表达（选中背景/前景色），不使用 radio 圆点（横条空间紧张）。
- 悬停 tooltip = 完整标签（如"中文输入 (Z)"）。

**FR-3 能力映射（必须与配图一致，不得增删）**

| 横条控件 | 等价菜单项 | 命令 ID | 语义 | 异常/离线行为 |
|---|---|---|---|---|
| 西文 | 西文输入 (&E) | `ID_WEASELTRAY_ENABLE_ASCII` **40013** | `SetOption(activeSession, "ascii_mode", **true**)` | 无宿主也可用（本地） |
| 中文 | 中文输入 (&Z) | `ID_WEASELTRAY_DISABLE_ASCII` **40014** | `SetOption(..., "ascii_mode", **false**)` | 同上 |
| 全角 | 全角符号 (&F) | `ID_WEASELTRAY_FULL_SHAPE` 40017 | `SetOption(..., "full_shape", true)` | 同上 |
| 半角 | 半角符号 (&H) | `ID_WEASELTRAY_HALF_SHAPE` 40018 | `SetOption(..., "full_shape", false)` | 同上 |
| 设置 | 打开设置 (&O) | `ID_WEASELTRAY_OIM_SETTINGS` 40019 | `Push("oim.open_panel", {"panel":"plugin.oim"})` | 宿主离线 → 气泡提示 |
| 同步 | 设备同步 (&S) | `ID_WEASELTRAY_OIM_SYNC` 40020 | `Push("oim.open_panel", {"panel":"plugin.sync"})` | 同上 |
| 翻译 | 文本翻译 (&T) | `ID_WEASELTRAY_OIM_TRANSLATE` 40021 | `Push("oim.open_translate", "{}")` | 同上 |

> **2026-10-13 二次迭代（用户要求）**：上表在 **UI 上合并为 5 个图标按钮** ——
> 中/英合并为**一个切换按钮**、全/半角合并为**一个切换按钮**，点击时按当前状态选择上表对应 ID
> （如当前中文 → 点它发 40014 西文；当前半角 → 点它发 40017 全角）；设置/同步/翻译为 3 个动作按钮。
> **能力映射关系不变**，托盘菜单项与命令 ID 也未动。

**FR-4 状态回显与实时同步**
- 横条中英/全角高亮必须反映**引擎实时状态**（`ascii_mode` / `full_shape`），数据源 = `OctStatus`（`RimeWithWeasel.cpp` 已产出 `schema_id/ascii_mode/full_shape/disabled/enabled`）。
- 状态变化来源包括：横条自身点击、托盘菜单点击、语言栏/TSF 触发、按键切换 — 全部要回显（复用 `_UpdateUI` 刷新路径）。
- 无活动会话时（理论上横条已隐藏）不得崩溃/误显。

**FR-5 命令路由（禁止重复实现）**
- 输入态两项：走既有 `ServerImpl::OnCommand` 的 40013/40014/40017/40018 分支（已存在）。
- 动作三项：把 `WeaselServerApp::SetupMenuHandlers` 里现成的三个 handler 抽成共用函数（如 `DoOimHostAction(UINT id)`），托盘与横条共用。

**FR-6 位置 / 多屏 / DPI**
- 默认位置：见 §9-Q1（建议"光标所在屏右下角工作区内边距 12px"）。
- 可拖拽（按住空白区拖动），松手吸附到当前屏边缘。
- 位置持久化：写入用户级配置（建议 `HKCU\Software\Rime\Weasel` 增加 `ToolbarPosition`，或 `%AppData%\Rime\oct_toolbar.json`）；按屏分别记忆或只记主屏（见 §9-Q1）。
- 多显示器（含混合 DPI）下不得出现在工作区外；随 `WM_DPICHANGED` / `WM_DISPLAYCHANGE` 校正。

**FR-7 窗口行为（硬要求）**
- 样式：`WS_POPUP | WS_CLIPSIBLINGS` + `WS_EX_TOOLWINDOW | WS_EX_TOPMOST | WS_EX_NOACTIVATE`（照抄 `WeaselPanel`，F4）。
- 不进 Alt-Tab、不进任务栏、不抢焦点（出现/点击都不改变宿主应用的前台窗口）。
- 置顶层级不得盖住候选窗（若位置冲突，候选窗优先，见 §10-R1）。

**FR-8 不干扰输入**
- 横条显隐、点击、拖拽**不得**中断 composition / 预编辑 / 上屏。
- 点击按钮后无需二次点击即可继续在原应用打字。

**FR-9 资源与主题**
- 图标复用现有资源：`IDI_ZH`(102) / `IDI_EN`(101) / `IDI_FULL_SHAPE`(106) / `IDI_HALF_SHAPE`(107)，**不新增 .rc 资源**（降低构建风险）。
- 主题跟随系统深浅色（可选，失败降级为浅色）。

**FR-10 失败降级**
- 横条窗口创建/绘制失败 → **仅保留托盘**，不得影响输入法本体功能，不得崩溃。
- 立即退出/注销时无残留窗口（挂在 `Run()` 生命周期内，随 `tray_icon.RemoveIcon()` 一并销毁）。

---

## 5. 技术落点（文件级）

| 文件 | 动作 |
|---|---|
| `mvp_test/oim/weasel/WeaselServer/OimToolbar.{h,cpp}` | **新增**：横条窗口类（窗口样式照抄 `WeaselPanel`；按钮自绘或子窗口；含 DPI 缩放与持久化读写） |
| `mvp_test/oim/weasel/WeaselServer/WeaselServerApp.cpp` | `Run()` 内创建/销毁横条；在**既有**的 `m_server.SetSessionActiveCallback(...)` 回调里，除 `tray_icon.SetImeActive(active)` 外一并驱动 `toolbar.SetImeActive(active)`（FR-1 同源）；把三个 OIM handler 抽成共用函数供托盘与横条调用（FR-5） |
| `mvp_test/oim/weasel/WeaselIPCServer/WeaselServerImpl.cpp` | **无需改**：40013/40014/40017/40018 分支已存在，横条按命令 ID 复用 `OnCommand` 路径 |
| `mvp_test/oim/weasel/WeaselServer/WeaselTrayIcon.{h,cpp}` | 仅作参照（`SetImeActive` / `Refresh` 守卫是显隐与刷新范式），不改行为 |
| 资源 | 复用 `IDI_*`（FR-9），不改 `.rc` 菜单 |
| 构建 | **仅 x64**：`msbuild weasel.sln /t:WeaselServer /p:Configuration=Release /p:Platform=x64`（Win32 通道缺 32 位 boost 预编译库，会 LNK1104，且失败清理会删 `output\oimime.dll`，详见 `HANDOFF.md` §四.4 / §五） |

**线程约束**：横条的一切窗口操作（创建/显隐/绘制/`Shell_NotifyIcon` 类调用）只在**服务器消息线程**执行；`OnSessionActive`（消息线程）→ 直接调用即可；引擎状态读取走 `ExecOnHandler`（持 `g_api_mutex`），禁止在消息线程直接碰 librime（复用托盘既有范式）。

---

## 6. 非目标（Out of scope）

- ❌ 不做 Electron 宿主侧工具条窗口（F2 已否决）
- ❌ 不改托盘菜单结构、文案与快捷键
- ❌ 不做 TSF 语言栏（`WeaselTSF/LanguageBar.cpp`）集成
- ❌ 不做横条皮肤/按钮自定义/配置面板；不做三语言文案（先简中，后续跟随 `.rc` 三语言段）
- ❌ 横条不承担候选/词库/翻页等输入操作
- ❌ 不新增 `OctServer` 协议方法（复用 `oim.open_panel` / `oim.open_translate`）

---

## 7. 验收标准（端到端可测）

| # | 场景 | 期望 |
|---|---|---|
| A1 | **不启动 OctPlugins 宿主**，切到「Oct 输入法」 | 横条出现 + 托盘出现（同一时刻） |
| A2 | 切到「微软拼音」等其他输入法 | 横条消失 + 托盘消失 |
| A3 | 横条点「西文」/「中文」 | 输入法立刻转西文/中文，高亮同步；**无需宿主** |
| A4 | 横条点「全角」/「半角」 | 状态即时切换并回显 |
| A5 | 宿主在线，点设置/设备同步/文本翻译 | 分别打开 oim 面板 / 设备同步面板 / 翻译新窗 |
| A6 | 宿主离线，点上述三项 | 气泡「宿主未运行，请先启动 OctPlugins 桌面端」 |
| A7 | 焦点与输入 | 横条出现/点击后，原应用仍是前台，可直接继续打字；composition 不丢 |
| A8 | 多屏 + 混合 DPI（100%/150%） | 横条位置正确、不出工作区 |
| A9 | 拖动横条 → 退出 oimd → 重启 | 位置保留 |
| A10 | 回归 | 候选窗、上屏、托盘菜单（40017-40021）行为不变；`output\oimime.dll` 未被误删 |

---

## 8. 风险与对策

| # | 风险 | 对策 |
|---|---|---|
| R1 | 横条与候选窗位置冲突（同屏右下） | 横条按候选窗位置避让，或固定边缘 + 允许拖动（§9-Q1 拍板） |
| R2 | 消息线程 / `g_api_mutex` 死锁 | 严格照抄托盘范式：窗口操作只在消息线程；引擎访问只经 `ExecOnHandler` |
| R3 | 高 DPI 下 GDI 绘制模糊 | 用 `WeaselUI/d2d.cpp` 或按 `GetDpiForWindow` 缩放字号与尺寸 |
| R4 | 横条抢焦点导致输入中断 | `WS_EX_NOACTIVATE` 且不调用 `SetForegroundWindow`；按钮用自绘命中测试而非标准 `BUTTON` 子窗口（标准子窗口会改焦点） |
| R5 | Win32 通道构建失败连带删产物 | 只走 x64 单目标构建（§5） |

---

## 9. 开放问题（已由用户拍板，实现按此执行）

- **Q1 默认位置** = **屏幕右下**（实现：光标所在屏的工作区右下，留 12px 边距；拖动后位置写入 `HKCU\Software\Rime\Weasel\OimToolbarPos` 并优先使用）。
- **Q2 关闭按钮** = **不要**（横条只随输入法激活态显隐，无手动关闭）。
- **Q3 品牌标识** = **要**，取自 `OctPlugins\resources\logo\logo32.png`（实现：复制为 `weasel\resource\oim_logo.png`，以 `IDR_OIM_LOGO` RCDATA 嵌入 oimd.exe，运行时 WIC 解码 + AlphaBlend 绘制；解码失败降级画 `IDI_ZH` 图标）。
- **Q4 rime 原菜单项** = 不放进横条（横条只承载配图 7 项对应的 5 个控件）。
- **Q5 方向** = **固定横向**（不做竖向/可旋转）。

---

## 10. 实现结果（2026-10-13）

**改动落点**

| 文件 | 动作 |
|---|---|
| `WeaselServer/OimToolbar.{h,cpp}` | **新增**：横条窗口类（自绘、无子窗口；窗口样式 `WS_EX_TOOLWINDOW\|TOPMOST\|NOACTIVATE` + `WS_POPUP\|CLIPSIBLINGS`；`WM_MOUSEACTIVATE→MA_NOACTIVATE`） |
| `WeaselServer/WeaselServerApp.{h,cpp}` | `Run()` 创建横条（失败 `reset()` 降级为仅托盘）；`SetSessionActiveCallback` 里与 `tray_icon.SetImeActive` 并列驱动；`OnUpdateUI` 里同步 `RequestRefresh()`；退出时 `Destroy()` |
| `WeaselServer/resource.h` / `WeaselServer.rc` | 新增 `IDR_OIM_LOGO`(108) + RCDATA（PNG 嵌入 exe） |
| `weasel/resource/oim_logo.png` | 由 `OctPlugins/resources/logo/logo32.png` 复制 |
| `WeaselServer/WeaselServer.vcxproj` | 加入 `OimToolbar.cpp` |
| `WeaselIPCServer/*`、`WeaselTrayIcon.*` | **未改**（横条复用既有 `WM_COMMAND`/`OnCommand` 与激活态回调） |

**命令出口**：横条只发 `WM_COMMAND(id, 0)` 给服务器窗口 → 与托盘菜单同一条 `ServerImpl::OnCommand` / `MenuHandler` 路径，横条内零业务逻辑（满足 D4/FR-5）。

**构建**：`msbuild weasel.sln /t:WeaselServer /p:Configuration=Release /p:Platform=x64` → EXIT=0（`oimd.exe` ≈2.85MB）。注意 Win32 通道仍缺 32 位 boost（见 HANDOFF §四.4）。
坑：`<windows.h>` 的 `max` 宏会打断 `std::max`（C2589），必须写 `(std::max)(...)`；且 `SIZE.cx` 是 `LONG`，需转 `int` 才与 `Scale()` 同类型。

**机制级验证（实测数据）**：直接向服务器窗口 `WeaselIPCWindow_1.0` 投递 `WM_WEASEL_SESSION_ACTIVE`（= `WM_APP+17+201`）模拟 TSF 会话跃迁：

```
before        : visible=False  size=10x10
post active(1): visible=True   size=386x36  pos=(1522,992)   ← 工作区右下，12px 边距
post inactive : visible=False  size=386x36
```

另核实：oimd 内同时存在 5 个窗口（`OimToolbarWnd` / `tooltips_class32` / `WeaselPanel` / `TrayIconClass` / `WeaselIPCWindow_1.0`），RPC `oim.status` 正常，`oimd.exe` 内含 PNG 签名（Logo 已嵌入）。

**仍需人工验收（需真实切换输入法）**：A1/A2（与托盘同时出现/消失）、A3/A4（中英/全角点击）、A5/A6（宿主在线/离线三动作）、A7（不抢焦点）、A9（拖动后位置保留）。

---

## 11. 二次迭代：图标化 + 切换按钮（2026-10-13 下午）

用户反馈："中英文、全角半角应该是切换按钮，而且最好用图标（像微信输入法的图标看着很舒服）"。

**形态变化**

```
改前：[Logo] │ 中文 西文 │ 全角 半角 │ 设置 同步 翻译     （7 个文字按钮，386×36）
改后：[Logo] │ [中/英⇄] [全/半⇄] │ [设置] [同步] [翻译]   （5 个图标按钮，205×36）
```

**图标清单（全部 GDI+ 矢量绘制，无二进制资源、任意 DPI 清晰）**

| 控件 | 图形 | 说明 |
|---|---|---|
| 中/英⇄ | 圆角芯片：中文=暗金底白「中」；西文=浅底深「A」 | 微信输入法同款"字符芯片"表达 |
| 全/半⇄ | 全角=句号环「。」；半角=小圆点「.」 | 传统输入法惯例，形状即状态 |
| 设置 | 三条滑杆 + 旋钮 | 现代扁平的 settings 图标 |
| 设备同步 | 环形箭头（弧 + 三角箭头） | 与"同步/循环"语义对应 |
| 文本翻译 | 「文」+ 金色「A」 | 翻译类通用图标 |
| Logo | 圆角小磁贴（裁剪 + 细描边） | 原图是白底方块，直接贴像"白贴纸"，裁圆后与"应用图标"观感一致 |

**实现要点**

- 引入 GDI+：`GdiplusStartup`（Create）/ `GdiplusShutdown`（Destroy）；`#pragma comment(lib, "gdiplus.lib")`。
  `Paint()` 结构 = `Graphics` 作用域画背景/分隔/按钮/图标 → GDI 画 Logo（裁剪 + `AlphaBlend`，保留预乘 BGRA 的透明）→ `Graphics` 作用域画磁贴描边。
- 切换按钮的 ID 选择在 `ActivateButton()`：`ascii ? ITEM_ZH : ITEM_EN`、`full ? ITEM_HALF : ITEM_FULL`，
  仍然只发 `WM_COMMAND`，**未新增任何业务逻辑**。
- Tooltip 跟随状态：切换按钮显示"当前态 +（点击切换）"，文案取自托盘菜单资源（自动本地化）。

**验证（截图核对，5 倍放大）**

- 中文 + 半角：Logo 磁贴 / 暗金「中」芯片 / 半角圆点 / 滑杆 / 环形箭头 / 文A 全部正常。
- 强制西文 + 全角（临时构建核对后已删除该临时代码并重建）：浅底「A」芯片、全角「。」环正常。
- 尺寸实测 **205×36**，仍在工作区右下 12px 边距处。

**已知取舍**：横条图标/文案为简中（短标签已不再需要，tooltip 才是本地化文案）；如需三语言图标无关文案，无需改动。

**二次迭代的 bug 与修正（同日）**：用户反馈"中英文切换没成功"。根因是**命令 ID 语义接反**（非点击问题）：

- `WeaselServerImpl::OnCommand` 与 `/ascii` CLI 的既有语义：`ID_WEASELTRAY_ENABLE_ASCII`(40013) → `ascii_mode=true` → **西文**；`DISABLE_ASCII`(40014) → `ascii_mode=false` → **中文**。
- 但 `plan-oim-toolbar-migrate-to-ime.md` 当初给 `.rc` 贴的标签是反的（中文输入 → ENABLE_ASCII），`WeaselTrayIcon::CustomizeMenu` 的勾选项也反了；本轮工具条枚举沿用该错误假设，导致"点中/英"发出的命令把状态设成**当前已有值** → 永远 no-op。
- 修正三处：① `.rc` 三个语言段的标签 ↔ ID 对齐（40013=西文输入/English input，40014=中文输入/Chinese input）；② `CustomizeMenu` 按 ascii_mode 勾选 40013，并按**显示顺序**传单选区端点（显示顺序 40014→40013 与 ID 大小顺序相反，否则区间反向、旧勾选清不掉）；③ `OimToolbar` 枚举改为 `ITEM_EN=40013 / ITEM_ZH=40014`。
- 验证：从构建出的 `oimd.exe` 里 `LoadMenu` 读出资源 → `id 40013 = English input (&E)`、`id 40014 = Chinese input (&Z)` ✓。

**遗留注意**：这两个命令经 `SetOption(lParam=0, ...)` 落到 `m_active_session`；若点击时恰好没有活跃会话（焦点不在文本框），命令是 no-op。工具条可见性与会话**计数**绑定（START/END_SESSION），而 `FocusOut` 只清 `m_active_session`，故存在"工具条在、命令无效"的窗口——若要彻底消除，可让 `SetOption(ipc_id==0)` 时对全部会话生效。
