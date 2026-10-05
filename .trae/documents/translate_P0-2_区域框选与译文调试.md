# translate P0-2 · 区域拉框选择 + 译文排障

## Context（为什么做）
P0-2 屏幕翻译的「预设区域」目前是数字输入框（X/Y/宽/高），用户无法据此调试，也拿不到译文。用户明确要求：
1. **预设区域改为 UI 手动拉框**（鼠标在屏幕上拖拽矩形选中区域）——已确认覆盖**仅主屏**、并把数字输入**完全替换**为框选。
2. 排查「无法获取译文」（允许我手动操控宿主实际 E2E）。

现状：宿主 [overlay:open](file:///d:/Project/ElectronProject/OctPlugins/ui/src/main.ts#L95-L105) 建的透明窗是**全穿透**（`setIgnoreMouseEvents(true)`），只能被动展示、无法捕获鼠标拖拽，需一个交互模式才能拉框。宿主已有 `ipcMain.handle("rpc")`（main.ts L332）可转发任意方法到内核，选区页可直接写 region，无需新增宿主回调链路。

`window_resizer.py` 绝对不动；只用 Go/HTML，尽量少改宿主。

## 方案

### 1. 宿主小改：`ui/src/main.ts` 的 `overlay:open` 支持 `interactive`
`payload.interactive===true` 时不穿透（可捕获拖拽）：
- 创建后用 `if (payload.interactive) win.setIgnoreMouseEvents(false); else win.setIgnoreMouseEvents(true,{forward:true});`，且保持 `focusable:false`（不抢焦点，鼠标事件仍会派发到非焦点窗）。
- 其余（transparent/alwaysOnTop/skipTaskbar/`overlay:env`/`enableLargerThanScreen`）不变。
- 仍只用主屏 `getPrimaryDisplay().bounds`（按用户已确认「仅主屏」）。

### 2. 新建 overlay 插件 `plugins/translate_overlay/`
- `manifest.json`：`id:"translate_overlay"`、`kind:"overlay"`、`type:"node"`、`entry:"main.js"`、`ui.entry:"ui/sel.html"`、`load_mode:"lazy"`、空 functions（镜像 colorpicker_overlay 结构），让内核登记、宿主 `overlay:open` 能查到 `ui.entry`。
- `main.js`：最小 idle 后端，`$/handshake` 后停留（对齐插件协议），确保插件可启动、不报错。
- `ui/sel.html` + 内联 JS/CSS：全屏透明页（nodeIntegration 已开）：
  - 监听 `overlay:env` 拿窗口原点 `(ox,oy)` 与 dpr。
  - 半透明遮罩 + 拖拽矩形（`mix-blend-mode:difference` 高亮，直观看到底下真实像素）。
  - mousedown/mousemove/mouseup 计算选区 —— region(DIP) = `{x: ox+startX, y: oy+startY, w: dx, h: dy}`（鼠标 up 时取 top-left 归一化）。
  - mouseup 后经 `require('electron').ipcRenderer.invoke('rpc','plugin.call',{pluginId:'translate',method:'app.set_region',params:{region}})` 写 region，再 `invoke('rpc','plugin.call',… 'app.region')` 校验回读。
  - Esc 取消；写成功后 `ipcRenderer.invoke('overlay:close',{pluginId:'translate_overlay'})` 自关。

### 3. translate UI 替换：`mvp_test/translate/ui/index.html` + `src/app.js`
把「预设区域」数字输入块替换为：
- 「框选区域」按钮 → `postMessage({type:'oct.overlay',action:'open',pluginId:'translate_overlay',interactive:true})`。
- 只读展示「当前区域 (x,y w×h) / maxW×maxH」（数据来自 `app.region`），空则提示「尚未框选」。
- 点按钮后对 `translate.app.region` 做短轮询（约 8s），检测到 region 变化即更新展示并停止——这样不新增宿主转发，唯一宿主改动仍是 `interactive`。
- 「屏幕识别 / 单次翻译」按钮逻辑保持不变（已接 `app.start{key}`），直接用框选到的 region。

### 4. 译文排障（执行阶段，手动操控宿主）
框选功能就绪后，我操作宿主做完整 E2E：
- 设置 region（经 overlay 或注入）→「识别」核对截图尺寸与 OCR 阅读顺序输出 →「翻译」核对译文。
- 若取不到译文，按链路逐层定位：region 是否落盘（`app.region` 回读）→ 截图是否非黑（BitBlt/CAPTUREBLT/DPI 坐标）→ OCR 是否出文本（rapidocr 状态/models）→ hy/opus 是否就绪（translate 探测/模型路径）→ invokeGateway 是否返回。
- 按根因修（优先复用已实现 tool，Go 侧为主），修完回归确认能出译文。

## 关键文件
- 改：`ui/src/main.ts`（overlay interactive，约 2 行）
- 改：`mvp_test/translate/ui/index.html`、`src/app.js`（数字输入块→框选；其余保留）
- 增：`plugins/translate_overlay/manifest.json`、`main.js`、`ui/sel.html`
- 复用：`mvp_test/translate/region.go` setRegion/getRegionView、`app.set_region`、宿主 `rpc` 桥、`overlay:env`、colorpicker_overlay 结构

## 验证
- `go build`/`go vet`：translate 与 kernel（若有改动）编译干净；`translate_overlay` 结构合法。
- 重启宿主（新 kerneld 若变、导入 translate/translate_overlay）。
- 手动 E2E：「框选区域」→ 屏幕上拖拽 → 后端 `app.region` 回读为所选矩形 → 「屏幕识别」出正确 OCR 顺序文本 → 「翻译」出对应译文。全程由我操作宿主核对，避免口头代 commit。
- 明确未完成项：多屏框选（本次仅主屏）、`window_resizer.py` 不改。