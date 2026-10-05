// OCTplugin 宿主主进程（MVP）：
//   1) 拉起 Go 内核 daemon（./kernel/kerneld.exe）
//   2) 从内核 stdout 读首个 auth 行 {port, token}
//   3) WebSocket 连接 localhost:port，首条消息发带 token 的 kernel.hello
//   4) 调 plugin.list → 结果在窗口显示。
export {};

const { app, BrowserWindow, dialog, ipcMain, globalShortcut, Tray, Menu, nativeImage, clipboard, screen } = require("electron");
const { spawn, execFile } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");
const WebSocket = require("ws");
// §19 派生错误码常量（由 pkg/protocol 经 go:generate 生成，勿手改）。EDiagByCode 运行时推导负数键。
const { EDiagByCode, EMessages } = require("./protocol-errors");

// 运行时根（外置环境根）：本文件编译到 ui/build/，故向上两级 = 项目根（开发时）。
// 打包布局把 kernel/plugins/runtime/state/resources/bin 镜像到 app resources 下，此时__dirname=resources/build，
// 同样向上两级即可命中（resources/build/../.. = app 上一级）。两种模式一致。
const RUNTIME_ROOT = path.join(__dirname, "..", "..");

const KERNEL_EXE = path.join(RUNTIME_ROOT, "kernel", "kerneld.exe");
const APP_LOGO = path.join(RUNTIME_ROOT, "resources", "logo", "logo128.png");
const TRAY_ICON = path.join(RUNTIME_ROOT, "resources", "icons", "logo16.png");
const TRAY_ICON_2X = path.join(RUNTIME_ROOT, "resources", "icons", "logo32.png");

let win = null;
let ws = null;
let kernelProc = null;
let tray = null;
let seq = 0;
let kernelAuth = null; // 内核地址/token，供新开插件子窗口复用

// ── 通用工具(tool)桥 ───────────────────────
// 宿主零硬编码：不承载任何 tool 具体逻辑，只负责经内核向 tool 进程转发调用（tool:invoke）
// 与把 tool 事件回吐给插件 iframe（tool:event）。
// tool 是与插件同级的进程实体（kind:"tool"，见内核 Manifest.Kind），由内核插件进程管理
// 层负责启停/回收；宿主经 WS → 内核 → tool 进程调用。toolId 即内核中的插件 id（manifest.id）。
// 状态缓存：tool 事件触发者往往是宿主代理发起，这里只维护最近一次事件以便回吐给调用插件。
let toolLastEvent = null;

// 通用 overlay 窗口管理（非取色专属）：任何插件都能请求宿主把它（kind:"overlay"）的 ui 页
// 开成全屏透明、置顶、鼠标穿透的窗口。由 pluginId 索引 window，以便关闭；由 toolId 索引
// 路由（tool 发的内核事件要投给哪个 overlay 窗）。
const overlayWindows: Map<string, InstanceType<typeof BrowserWindow>> = new Map();
const overlayRoute: Map<string, InstanceType<typeof BrowserWindow>> = new Map(); // toolId → overlay window

// 宿主主进程侧不直接 require 任何 .node，也不持有 overlay 取色逻辑。取色等能力由
// napi_rs_tool 经插件进程通道提供；放大镜 overlay 是 kind:"overlay" 插件，由宿主把其 ui 页
// 开成全屏透明窗渲染（透明放大镜），取色业务编排在 colorpicker 插件内。

// 通用 tool 桥：经内核转发给 toolId 对应进程（复用 rpc() 到内核的通道）。
// tool 是被内核按插件进程管理的 "kind":"tool" 实体（见内核 Manifest.Kind），故经
// plugin.call 调用其方法；toolId = manifest.id。
ipcMain.handle("tool:invoke", async (e, payload) => rpc("plugin.call", {
  pluginId: payload && payload.toolId,
  method: payload && payload.method,
  params: payload && payload.params,
}, 20000));
// 通用 tool 事件回吐：宿主把内核广播到 tool 的事件转给请求订阅的插件（MVP 由调用方登记）。
ipcMain.handle("tool:subscribe", async (e, payload) => {
  toolLastEvent = payload || null;
  return { ok: true };
});

// 通用剪贴板写入：插件 iframe 在全屏 overlay 取色期间不是焦点文档，
// navigator.clipboard.writeText 会被 Chromium 静默拒绝；主进程 clipboard 与焦点无关。
ipcMain.handle("clipboard:write", async (e, payload: any = {}) => {
  const text = payload && payload.text;
  if (typeof text !== "string") return { ok: false, error: "缺 text" };
  try { clipboard.writeText(text); return { ok: true }; }
  catch (err: any) { return { ok: false, error: String((err && err.message) || err) }; }
});

// 通用 overlay：把某插件（kind:"overlay"）的 ui 页开成全屏透明、置顶、鼠标穿透窗口。
// pluginId 用于定位/关闭窗口；toolId 用于把该 tool 发来的内核事件路由到此 overlay 窗。
ipcMain.handle("overlay:open", async (e, payload: any = {}) => {
  const pluginId = payload.pluginId;
  if (!pluginId) return { ok: false, error: "缺 pluginId" };
  return openOverlayInternal(pluginId, {
    toolId: payload.toolId, interactive: !!payload.interactive, persistent: !!payload.persistent,
  });
});
// openOverlayInternal 主进程内部复用：把某插件（kind:"overlay"）的 ui 页开成全屏透明、置顶窗口。
// 供 ipc overlay:open 与热键专用编排（translate::realtime 先框选）共用同一条开窗路径。
async function openOverlayInternal(pluginId: string, opts: any = {}) {
  const toolId = opts.toolId;
  // 只允许已登记的 overlay(kind) 插件？内核清单里取 ui 入口；找不到则拒。
  let entry = "";
  try {
    const r = await rpc("plugin.list", {});
    const found = (r && r.plugins || []).find((p: any) => p.pluginId === pluginId);
    entry = found && found.ui && found.ui.entry || "";
  } catch (err) { /* 下面按空 entry 处理 */ }
  const base = path.join(RUNTIME_ROOT, "plugins", pluginId);
  const file = entry ? (path.isAbsolute(entry) ? entry : path.join(base, entry)) : "";
  if (!entry) return { ok: false, error: "未找到插件 ui 入口: " + pluginId };

  let ovWin = overlayWindows.get(pluginId);
  if (ovWin && !ovWin.isDestroyed()) { return { ok: true }; } // 已开
  const bounds = screen.getPrimaryDisplay().bounds;
  ovWin = new BrowserWindow({
    x: bounds.x, y: bounds.y,
    width: bounds.width, height: bounds.height,
    transparent: true, frame: false, resizable: false, movable: false,
    alwaysOnTop: true, skipTaskbar: true, hasShadow: false,
    focusable: false, enableLargerThanScreen: true, // 不抢焦点：避免 overlay 让宿主应用到失焦
    backgroundColor: "#00000000",
    webPreferences: { nodeIntegration: true, contextIsolation: false },
  });
  ovWin.setAlwaysOnTop(true, "screen-saver");
  // 全穿透：点按语义由取色 native 钩子处理。interactive=true（如区域拉框）时不穿透、捕获鼠标拖拽；
  // 保持 focusable:false —— 不抢宿主应用焦点，鼠标事件仍会派发到非聚焦窗。
  if (opts.interactive) {
    ovWin.setIgnoreMouseEvents(false);
  } else {
    ovWin.setIgnoreMouseEvents(true, { forward: true });
  }
  // 把 overlay 窗口原点 + 光标所在屏的缩放比推给渲染进程，用于把光标虚拟坐标换算到窗口坐标。
  const pushEnv = () => {
    try {
      const pt = screen.getCursorScreenPoint();
      const disp = screen.getDisplayNearestPoint(pt);
      const [ox, oy] = ovWin.getPosition();
      ovWin.webContents.send("overlay:env", { x: ox, y: oy, dpr: disp.scaleFactor });
    } catch (e) {}
  };
  ovWin.webContents.on("did-finish-load", pushEnv);
  screen.on("display-metrics-changed", () => pushEnv());
  ovWin.on("closed", () => screen.removeListener("display-metrics-changed", pushEnv));
  ovWin.on("closed", () => {
    overlayWindows.delete(pluginId);
    if (overlayRoute.get(toolId) === ovWin) overlayRoute.delete(toolId);
  });
  // persistent=1：字幕常驻框——开窗即不关闭，页面自行在 pick(拉框) / live(常驻金边) 两阶段切换。
  ovWin.loadFile(file, { query: { persistent: opts.persistent ? "1" : "" } });
  overlayWindows.set(pluginId, ovWin);
  if (toolId) overlayRoute.set(toolId, ovWin);
  return { ok: true };
}
// 关闭并注销 overlay 窗口（ipc 与主进程内部编排共用）。
ipcMain.handle("overlay:close", async (e, payload: any = {}) => closeOverlayInternal(payload.pluginId));
function closeOverlayInternal(pluginId: string) {
  const ovWin = overlayWindows.get(pluginId);
  if (ovWin && !ovWin.isDestroyed()) ovWin.destroy();
  overlayWindows.delete(pluginId);
  overlayRoute.forEach((v, k) => { if (v === ovWin) overlayRoute.delete(k); });
  return { ok: true };
}
// 常驻框动态穿透：capture=true 时窗口抓住鼠标（贴住金边拖动/缩放）；
// false 时整窗鼠标穿透（forward 仍把 mousemove 投给渲染进程做贴边命中），不挡被翻译软件操作。
ipcMain.on("overlay:set-capture", (e, capture: boolean) => {
  const w = BrowserWindow.fromWebContents(e.sender);
  if (w && !w.isDestroyed()) w.setIgnoreMouseEvents(!capture, { forward: !capture });
});
// 切换常驻框阶段：pick（拉框/微调模态）↔ live（金边常驻、动态穿透）。
ipcMain.handle("overlay:phase", async (e, payload: any = {}) => {
  const win = overlayWindows.get(payload.pluginId);
  if (!win || win.isDestroyed()) return { ok: false, error: "no-overlay" };
  try { win.webContents.send("overlay:phase", payload.phase || "pick"); return { ok: true }; }
  catch (err) { return { ok: false, error: String((err && err.message) || err) }; }
});

// 给宿主主进程一个可辨认的标题（任务管理器/进程列表里更容易区分）
process.title = "OCTplugin · 墨韵工作台";

// 插件请求以独立窗口打开页面（如 md 编辑器）。宿主把内核地址放查询参数，
// 子页 app.js 见 ?mdEditor=1&kport=&auth= 时自连内核 WS。
ipcMain.handle("win:openPluginWindow", async (e, opts) => {
  const url = opts && opts.url;
  if (!url || !kernelAuth) return { ok: false, error: !kernelAuth ? "内核未就绪" : "无 url" };
  const sep = url.includes("?") ? "&" : "?";
  const full = url + sep + "auth=" + encodeURIComponent(kernelAuth.token) + "&kport=" + kernelAuth.port;
  spawnPluginWindow(full, opts);
  return { ok: true };
});

// spawnPluginWindow 通用插件窗创建（win:openPluginWindow 与热键呈现共用）：
// frameless 时注入 editor-preload（自绘标题栏 + octWin）。仅建窗、不做单例/登记。
function spawnPluginWindow(full: string, opts: any) {
  const sub = new BrowserWindow({
    width: (opts && opts.width) || 960, height: (opts && opts.height) || 720,
    title: (opts && opts.title) || "插件窗口", icon: APP_LOGO,
    frame: !(opts && opts.frameless), // 自绘标题栏：frameless:true → frame:false（去掉原生标题栏）+ preload 注入
    autoHideMenuBar: true,
    webPreferences: {
      nodeIntegration: false, contextIsolation: true,
      preload: opts && opts.frameless ? require("path").join(__dirname, "editor-preload.js") : undefined,
    },
  });
  const emitSubState = () => { try { sub.webContents.send("win:state", sub.isMaximized() ? "max" : "normal"); } catch (e) {} };
  sub.on("maximize", emitSubState);
  sub.on("unmaximize", emitSubState);
  sub.loadURL(full);
  return sub;
}

// 子窗口窗口控制：自绘标题栏按钮 → 最小化 / 最大化(切换) / 关闭
ipcMain.on("win:ctrl", (e, act) => {
  const w = BrowserWindow.fromWebContents(e.sender);
  if (!w) return;
  if (act === "min") w.minimize();
  else if (act === "max") w.isMaximized() ? w.unmaximize() : w.maximize();
  else if (act === "close") w.close();
});

// 设置子窗口：在主窗口外以独立小窗打开“某一项设置表单”（parent:win 使关闭主窗时一并清理）。
// 复用 win:openPluginWindow 的建窗思路；URL 追加 view=settings&sec=…&auth=&kport=（sub 的 index.html 走宿主 rpc 桥，auth/kport 仅为格式一致）
ipcMain.handle("win:openSettings", async (e, { sec }: any = {}) => {
  if (!kernelAuth) return { ok: false, error: "内核未就绪" };
  const names: { [k: string]: string } = { A: "依赖源设置", B: "启动与资源·全局策略", C: "启动与资源·单插件策略", D: "插件排序·默认页" };
  const name = names[sec] || sec || "设置";
  const sub = new BrowserWindow({
    width: 720, height: 640, title: "设置 · " + name, icon: APP_LOGO,
    autoHideMenuBar: true, parent: win, // parent 绑定但仍是可拖出主窗外的独立窗口
    // 与主窗一致开启 nodeIntegration：子窗加载的是同一个 index.html，其渲染脚本用 require("electron") +
    // ipcRenderer.invoke("rpc") 经由宿主主进程复用内核连接（详见 index.html 注释）。
    webPreferences: { nodeIntegration: true, contextIsolation: false },
  });
  // 必须用 loadFile 的 options.query 传参：直接把 ?view=… 拼进 filePath 会被当磁盘文件名，进不了子窗口模式。
  sub.loadFile("index.html", {
    query: { view: "settings", sec: sec || "", auth: kernelAuth.token, kport: String(kernelAuth.port) },
  });
  // 设置窗口关闭时兜底重注册热键（若用户正在录制组合键，captureStart 已把全局键注销）
  sub.on("closed", () => { try { applyHotkeys().catch(() => {}); } catch (e) { /* ignore */ } });
  return { ok: true };
});

// 关闭当前调用者所属窗口（设置子窗口的“关闭”按钮用；sender 即子窗自身）
ipcMain.handle("win:closeSelf", (e) => {
  const w = BrowserWindow.fromWebContents(e.sender);
  if (w) w.close();
  return { ok: true };
});

// 重启应用：导入新插件后宿主通常需重建内核/刷新侧栏，直接重启最省事。
ipcMain.handle("app:relaunch", () => {
  app.relaunch();
  app.quit();
  return { ok: true };
});

function showMain() {
  if (!win) return;
  win.show(); win.focus();
}
function setupTray() {
  // 托盘图标用 logo128.png（与窗口/顶栏同款 logo）；异常则回退小尺寸 PNG
  let img = null;
  try { img = nativeImage.createFromPath(APP_LOGO); } catch (e) { img = null; }
  if (!img || img.isEmpty()) img = nativeImage.createFromPath(fs.existsSync(TRAY_ICON) ? TRAY_ICON : TRAY_ICON_2X);
  tray = new Tray(img);
  tray.setToolTip("OCTplugin · 墨韵工作台");
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: "显示主窗口", click: showMain },
    { type: "separator" },
    { label: "退出", click: () => app.quit() },
  ]));
  tray.on("click", showMain);
}

function createWindow() {
  win = new BrowserWindow({
    width: 900,
    height: 560,
    title: "OCTplugin · MVP",
    icon: APP_LOGO,
    frame: false, // 完全无边框：自绘顶部栏（拖拽区 + 窗口控制按钮），去掉原生重复的标题栏
    webPreferences: { nodeIntegration: true, contextIsolation: false },
  });
  win.loadFile("index.html");
  // 生命周期契约：主窗销毁后立即把引用置空。所有访问点均以 `if (win)` 判活，
  // 避免关闭阶段异步回调（内核 WS 事件等）打到已销毁窗口抛 "Object has been destroyed"。
  win.on("closed", () => { win = null; });
  // 自绘标题栏的窗口控制
  ipcMain.on("win:minimize", () => win && win.minimize());
  ipcMain.on("win:maximize", () => {
    if (!win) return;
    win.isMaximized() ? win.unmaximize() : win.maximize();
  });
  ipcMain.on("win:close", () => win && win.close());
  ipcMain.on("win:tray", () => win && win.hide()); // 收起到系统托盘
  const emitState = () => { try { win.webContents.send("win:state", win.isMaximized() ? "max" : "normal"); } catch (e) {} };
  win.on("maximize", emitState);
  win.on("unmaximize", emitState);
  // 等 renderer 完成加载后再启动链路，避免 send 早于监听而丢失
  win.webContents.on("did-finish-load", () => {
    if (win._booted) return;
    win._booted = true;
    emitState();
    bootstrap();
  });
}

// ── 一方插件默认放权 ──────────────────────────────────────────────────
// 权限模型的本意是「用户授权」，所以信任名单必须由**宿主**持有。绝不能让插件在
// manifest 里自己声明 auto_grant —— 那等于插件给自己发权限，perms.json 就形同虚设。
// 名单内插件按其 manifest **声明的全部权限**放权（不越声明范围）；已授权则不重复写。
// 想长期收回某个插件的权限，把它从这份名单里删掉再去设置页撤销。
const FIRST_PARTY_PLUGINS = ["translate"]; // 一方插件自动放权名单（translate 声明 file/local_model/screen/mic）

async function autoGrantFirstParty() {
  for (const pid of FIRST_PARTY_PLUGINS) {
    try {
      const det = await rpc("plugin.details", { pluginId: pid }, 15000);
      const decl = (det && det.permissionsDeclared) || [];
      const granted = (det && det.permissionsGranted) || [];
      const missing = decl.filter((p) => !granted.includes(p));
      if (!missing.length) continue;
      await rpc("perms.authorize", { pluginId: pid, perms: missing }, 15000);
      console.log(`[host] 一方插件 ${pid} 已放权：${missing.join(", ")}`);
    } catch (e) {
      console.warn(`[host] 一方插件 ${pid} 放权失败：`, e.message);
    }
  }
}

async function bootstrap() {
  try {
    const auth = await startKernel();
    kernelAuth = auth; // 供后续创建插件子窗口（md 编辑器等）复用端口/token
    await connect(auth);
    await applyHotkeys(); // 阶段L：内核连上后再注册全局热键（触发时要经 plugin.call 派发）
    await autoGrantFirstParty(); // 一方插件放权须早于首屏，避免自检显示未授权
    const list = await rpc("plugin.list", {});
    // 判活：启动链路耗时期间用户可能已关窗
    if (win && !win.isDestroyed()) win.webContents.send("kernel:ready", {
      list,
      kernelBase: `http://127.0.0.1:${auth.port}/`,
      resBase: `http://127.0.0.1:${auth.port}/res/`,
      port: auth.port, token: auth.token,
    });
    // §27：启动早期经「打开方式」收到的文件，内核就绪后再补发（renderer 面板此时已装配）。
    if (pendingOpenFile) {
      const f = pendingOpenFile; pendingOpenFile = "";
      try { if (win && !win.isDestroyed()) win.webContents.send("file:open", { path: f }); } catch (e) { /* 忽略 */ }
    }
    // §7 默认模型预加载（转化域 §四：加载权在宿主）：不阻塞首屏，后台确保默认能力模型就绪。
    // 每能力选默认最快模型（MVP：ocr=rapidocr，其余能力有模型声明后自动覆盖）。
    preloadDefaultModels();
  } catch (e) {
    console.error("启动失败:", e);
    if (win && !win.isDestroyed()) win.webContents.send("kernel:error", String(e));
  }
}

// §7.1/§7.2 默认模型预加载：宿主启动时每能力加载默认最快模型（写 EffectiveConfig 前 MVP 用
// registry.json.models 声明驱动：每个 capability 取第一个模型 acquire 就绪登记）。
// 失败仅降级告警，不阻塞应用启动。
async function preloadDefaultModels() {
  try {
    const s = await rpc("ui.getSettings", {}, 5000);
    const ui = (s && s.ui) || {};
    if (ui.preloadModels === false) { console.log("[preload] 用户已关闭默认模型预加载，跳过"); return; }
    const r = await rpc("registry.models.list", {}, 15000);
    const models = (r && r.models) || [];
    const byCap = new Map(); // capability → modelId（按声明顺序取第一个为默认）
    for (const mo of models) {
      if (!mo.capability) continue;
      if (mo.companion) continue; // 伴随模型随主模型一起加载（§四），不单独预加载
      if (!byCap.has(mo.capability)) byCap.set(mo.capability, mo.id);
    }
    for (const [cap, id] of byCap) {
      try { await rpc("registry.models.acquire", { id }, 30000); }
      catch (err) { console.warn(`[preload] ${cap} 默认模型 ${id} 就绪失败:`, String((err && err.message) || err)); }
    }
  } catch (err) {
    console.warn("[preload] 模型预加载跳过:", String((err && err.message) || err));
  }
}

// renderer 通用 RPC 桥（授权/吊销/读文件等）
ipcMain.handle("rpc", async (e, method, params, timeoutMs) => rpc(method, params, timeoutMs));

// 阶段1：选择插件源目录（「添加插件」导入用）
ipcMain.handle("dialog:pickDir", async () => {
  const { canceled, filePaths } = await dialog.showOpenDialog(win, {
    title: "选择插件源目录", properties: ["openDirectory"],
  });
  if (canceled || !filePaths.length) return { canceled: true };
  return { canceled: false, path: filePaths[0] };
});

// 文件对话框：供插件选择单个文件（如格式转换的源文件）；filters=[{name,extensions}]
ipcMain.handle("dialog:pickFile", async (e, filters) => {
  const { canceled, filePaths } = await dialog.showOpenDialog(win, {
    title: "选择文件",
    properties: ["openFile"],
    filters: (filters && filters.length ? filters : null) || [{ name: "所有文件", extensions: ["*"] }],
  });
  if (canceled || !filePaths.length) return { canceled: true };
  return { canceled: false, path: filePaths[0] };
});

// 保存对话框：供插件指定「另存为」目标（如格式转换的输出文件）。
// opts = { filters?: [{name,extensions}], defaultPath?: string }
ipcMain.handle("dialog:saveFile", async (e, opts) => {
  const o = opts && typeof opts === "object" ? opts : {};
  const filters = (o.filters && o.filters.length ? o.filters : null) || [{ name: "所有文件", extensions: ["*"] }];
  const { canceled, filePath } = await dialog.showSaveDialog(win, {
    title: o.title || "保存为",
    defaultPath: o.defaultPath || undefined,
    filters,
  });
  if (canceled || !filePath) return { canceled: true };
  return { canceled: false, path: filePath };
});

// ── 外部地址与内存设置：扫描各插件静态 manifest（不依赖插件进程是否在线）──
// §17.3：用户态（models.json）一律经内核 RPC 读写，宿主不直连 state/plugins/*。manifest 声明
// 仍取自插件包 plugins/<id>/resources/manifest.json（§5.1：插件包只读、宿主可扫描）。
const PLUGINS_ROOT = path.join(RUNTIME_ROOT, "plugins");
const LOAD_DEF = "lazy", UNLOAD_DEF = "idle", IDLE_MIN_DEF = 10;
function readJsonSafe(p, fallback) {
  try { return JSON.parse(fs.readFileSync(p, "utf8")); } catch (e) { return fallback; }
}
// 经内核读某插件已保存的资源配置（state/plugins/<pid>/models.json）。
async function readSavedResources(pid) {
  try {
    const r = await rpc("plugin.resources", { pluginId: pid }, 10000);
    return (r && r.resources) || {};
  } catch (e) { return {}; }
}
ipcMain.handle("res:scan", async () => {
  const groups = [];
  if (!fs.existsSync(PLUGINS_ROOT)) return { groups };
  const dirs = fs.readdirSync(PLUGINS_ROOT, { withFileTypes: true }).filter(d => d.isDirectory());
  for (const d of dirs) {
    const pid = d.name;
    const mf = path.join(PLUGINS_ROOT, pid, "resources", "manifest.json");
    if (!fs.existsSync(mf)) continue;
    const manifest = readJsonSafe(mf, []);
    const resources = await readSavedResources(pid);
    for (const m of manifest) {
      const rec = resources[m.key] || {};
      groups.push({
        pid, key: m.key, label: m.label || m.key, type: m.type || "model",
        variant: m.variant || "", path: rec.path || "", load: rec.load || LOAD_DEF,
        unload: rec.unload || UNLOAD_DEF, idle_min: rec.idle_min || IDLE_MIN_DEF,
      });
    }
  }
  return { groups };
});
ipcMain.handle("res:set", async (e, { pid, item }: any = {}) => {
  if (!pid || !item || !item.key) return { ok: false, error: "缺少 pid / item.key" };
  // §17.3/§5.1：由内核写入 state/plugins/<pid>/models.json（宿主不直连文件，用户数据不进插件包 store/）。
  const payload = {
    key: item.key, path: item.path || "",
    load: item.load || LOAD_DEF, unload: item.unload || UNLOAD_DEF,
    idle_min: (item.idle_min === undefined || item.idle_min === null || item.idle_min === "")
      ? IDLE_MIN_DEF : Math.max(1, parseInt(item.idle_min, 10) || IDLE_MIN_DEF),
  };
  try {
    const r = await rpc("plugin.setResources", { pluginId: pid, item: payload }, 10000);
    if (!r || !r.ok) return { ok: false, error: "内核拒绝保存资源设置" };
    return { ok: true };
  } catch (err) {
    return { ok: false, error: String((err && err.message) || err) };
  }
});

// ── §27 文件关联（Windows「打开方式」） ─────────────────────────────
// 宿主启动时向 HKCU\Software\Classes 静默注册「打开方式」项（无需管理员权限）：
//   1) Applications\octplugin.exe → 出现在「打开方式 → 选择其他应用」；
//   2) 每扩展名 OpenWithProgids → 直接出现在右键「打开方式」子菜单。
// 用户右键文件 → 打开方式 → 本应用 → Windows 以「<exe> <file>」拉起 → 单实例转发
// 文件路径给主实例 → 切 conversion 面板列出可达目标格式并允许直接转换（conversion.md §27）。
const FA_APP = "octplugin.exe"; // Applications 子键名（与打包后 exe 文件名一致）
const FA_PROGID = "OctPlugins.OpenWith"; // 打开方式 ProgID
// 打开方式命令：打包后直接以应用 exe 打开；开发模式经 electron.exe + 入口脚本。
function faEntryCommand() {
  if (app.isPackaged) return `"${app.getPath("exe")}" "%1"`;
  return `"${process.execPath}" "${path.join(__dirname, "main.js")}" "%1"`;
}
// 扫描各 tool manifest 声明的转换源格式（conversion.md §二：边由 tool 声明）。
function faScanExtensions() {
  const set = new Set<string>();
  if (!fs.existsSync(PLUGINS_ROOT)) return [];
  const dirs = fs.readdirSync(PLUGINS_ROOT, { withFileTypes: true })
    .filter(d => d.isDirectory() && !d.name.startsWith("_") && !d.name.startsWith("."))
    .map(d => d.name);
  for (const pid of dirs) {
    const mf = readJsonSafe(path.join(PLUGINS_ROOT, pid, "manifest.json"), null);
    if (!mf || !Array.isArray(mf.conversions)) continue;
    for (const c of mf.conversions) {
      for (const f of c.from || []) {
        const e = String(f).toLowerCase().replace(/^\./, "").trim();
        if (e) set.add(e);
      }
    }
  }
  return [...set].sort();
}
// 单条注册（reg add，HKCU 无需提权）；失败仅记录，不阻断启动。
function regAdd(key: string, data: string, value = "/ve"): Promise<boolean> {
  return new Promise((res) => {
    execFile("reg", ["add", key, value, "/d", data, "/f"], { windowsHide: true }, (err) => res(!err));
  });
}
async function registerFileAssoc() {
  const cmd = faEntryCommand();
  const exts = faScanExtensions();
  const failed: string[] = [];
  if (!(await regAdd(`HKCU\\Software\\Classes\\Applications\\${FA_APP}\\shell\\open\\command`, cmd))) failed.push("Applications\\command");
  if (!(await regAdd(`HKCU\\Software\\Classes\\${FA_PROGID}\\shell\\open\\command`, cmd))) failed.push("ProgID\\command");
  if (!(await regAdd(`HKCU\\Software\\Classes\\${FA_PROGID}\\DefaultIcon`, `"${app.getPath("exe")}",0`))) failed.push("ProgID\\DefaultIcon");
  for (const e of exts) {
    if (!(await regAdd(`HKCU\\Software\\Classes\\.${e}\\OpenWithProgids\\${FA_PROGID}`, ""))) failed.push("." + e);
  }
  if (failed.length) console.warn("[fileassoc] 部分「打开方式」注册失败:", failed.join(", "));
}
// 从命令行参数提取用户经「打开方式」传入的文件路径（跳过开关、exe、入口脚本）。
function extractOpenFile(argv: string[]): string {
  const entry = app.isPackaged ? "" : path.resolve(path.join(__dirname, "main.js"));
  for (const a of argv || []) {
    if (!a || a.startsWith("-")) continue;
    const p = path.resolve(String(a).replace(/^--/, ""));
    if (entry && p === entry) continue;
    if (/\.(exe|dll|node|bin)$/i.test(p)) continue;
    try { if (fs.statSync(p).isFile()) return p; } catch (e) { /* 非文件参数忽略 */ }
  }
  return "";
}
let pendingOpenFile = ""; // bootstrap 完成前收到的打开请求，就绪后补发
function handleOpenFile(p: string) {
  if (!win) return;
  win.show(); win.focus();
  if (win._booted) {
    try { win.webContents.send("file:open", { path: p }); } catch (e) { /* 窗口未就绪则稍后由 bootstrap 补发 */ }
  } else {
    pendingOpenFile = p;
  }
}

// ── 阶段L · 全局/应用内热键（FR-1 声明 → FR-9 管理）────────────────────────
// 声明源：plugins/<id>/manifest.json 的 hotkeys[]（静态文件，每次实时扫描，无缓存）
// 用户态：state/hotkeys.json（"<pluginId>::<hotkeyId>" → { combo, enabled }），归设置面板管
// 职责：宿主主进程持有 globalShortcut 注册表（注册/冲突检测/触发派发）；
//       插件只负责「声明」，不自己注册全局键；app 作用域由渲染进程 keydown 处理。
const MOD_ORDER = ["Ctrl", "Alt", "Shift", "Meta"];
const FKEYS = new Set(Array.from({ length: 24 }, (_, i) => "F" + (i + 1)));
const NAMED_KEYS = new Set(["Space", "Esc", "Enter", "Tab", "Backspace", "Delete", "Insert",
  "Home", "End", "PageUp", "PageDown", "Up", "Down", "Left", "Right", "Plus"]);
// KeyboardEvent.key → accelerator 键名
const KEY_ALIAS = {
  " ": "Space", "Escape": "Esc", "ArrowUp": "Up", "ArrowDown": "Down",
  "ArrowLeft": "Left", "ArrowRight": "Right", "Enter": "Enter", "Tab": "Tab",
  "Backspace": "Backspace", "Delete": "Delete", "Insert": "Insert",
  "Home": "Home", "End": "End", "PageUp": "PageUp", "PageDown": "PageDown",
};
const hkRegistered = new Map(); // 已注册 accelerator → { pluginId, id }
let hkReport = [];

function normalizeKeyToken(k) {
  if (!k) return "";
  if (/^[a-z]$/i.test(k)) return k.toUpperCase();
  if (/^[0-9]$/.test(k)) return k;
  if (FKEYS.has(String(k).toUpperCase())) return String(k).toUpperCase();
  const alias = KEY_ALIAS[k];
  if (alias) return alias;
  if (NAMED_KEYS.has(k)) return k;
  return "";
}
// normCombo 把任意写法（"alt+x" / "Alt + X" / "Control+X"）归一为 "Alt+X"；
// 非法返回 ""。全局热键要求「至少一个修饰键」或功能键（避免劫持裸字母）。
function normCombo(raw) {
  if (!raw || typeof raw !== "string") return "";
  const toks = raw.split("+").map(t => t.trim()).filter(Boolean);
  if (!toks.length) return "";
  const mods = new Set();
  let keyTok = "";
  for (const t of toks) {
    const lt = t.toLowerCase();
    if (lt === "ctrl" || lt === "control") { mods.add("Ctrl"); continue; }
    if (lt === "alt" || lt === "option" || lt === "altgr") { mods.add("Alt"); continue; }
    if (lt === "shift") { mods.add("Shift"); continue; }
    if (lt === "meta" || lt === "cmd" || lt === "command" || lt === "super" || lt === "win") { mods.add("Meta"); continue; }
    if (keyTok) return "";
    keyTok = normalizeKeyToken(t);
    if (!keyTok) return "";
  }
  if (!keyTok) return "";
  if (!FKEYS.has(keyTok) && mods.size === 0) return "";
  return [...MOD_ORDER.filter(m => mods.has(m)), keyTok].join("+");
}

function readHotkeyStore() {
  return { version: 1, items: {} }; // 由内核持有 state/hotkeys.json；此处仅占位，读走 readHotkeyStoreAsync
}
// 读用户热键覆盖：经内核 RPC hotkeys.get（§17.3：宿主不直连 state/hotkeys.json）。
async function readHotkeyStoreAsync() {
  try {
    const r = await rpc("hotkeys.get", {}, 10000);
    if (r && r.items && typeof r.items === "object") {
      return { version: r.version === undefined ? 1 : r.version, items: r.items };
    }
  } catch (e) { /* 内核不可用则按空表处理 */ }
  return { version: 1, items: {} };
}
// 写用户热键覆盖：经内核 RPC hotkeys.set（§17.3）。
async function writeHotkeyStoreAsync(items) {
  await rpc("hotkeys.set", { version: 1, ...items }, 10000);
}
// writeHotkeyStore 兼容占位（新增代码一律走 writeHotkeyStoreAsync）。
function writeHotkeyStore(data) { throw new Error("直接写 hotkeys 被禁止（§17.3），改用内核 RPC"); }

// scanDeclaredHotkeys 动态扫描全部插件 manifest.json 的 hotkeys 声明。
function scanDeclaredHotkeys() {
  const out = [];
  if (!fs.existsSync(PLUGINS_ROOT)) return out;
  const dirs = fs.readdirSync(PLUGINS_ROOT, { withFileTypes: true })
    .filter(d => d.isDirectory() && !d.name.startsWith("_") && !d.name.startsWith("."))
    .map(d => d.name).sort();
  for (const pid of dirs) {
    const mf = readJsonSafe(path.join(PLUGINS_ROOT, pid, "manifest.json"), null);
    if (!mf || !Array.isArray(mf.hotkeys)) continue;
    for (const hk of mf.hotkeys) {
      if (!hk || !hk.id) continue;
      const rawCombo = hk.combo || "";
      const hkw = hk.window;
      out.push({
        key: pid + "::" + hk.id, pluginId: pid, pluginName: mf.name || pid, id: hk.id,
        name: hk.name || hk.id, desc: hk.desc || "",
        rawCombo, defaultCombo: normCombo(rawCombo),
        scope: hk.scope === "app" ? "app" : "global",
        method: hk.method || "", action: hk.action || "",
        params: hk.params || {}, timeoutMs: hk.timeoutMs || 0,
        // 呈现窗声明（通用字段；宿主不识别插件专用逻辑）
        hwin: hkw && hkw.page ? {
          page: hkw.page, title: hkw.title || hk.name || hk.id,
          width: hkw.width || 480, height: hkw.height || 320,
          frameless: !!hkw.frameless, singleton: hkw.singleton !== false,
        } : null,
      });
    }
  }
  return out;
}

// resolveHotkeys 合并「manifest 声明」+「用户覆盖」，得到最终生效热键表（经内核读覆盖，§17.3）。
async function resolveHotkeys() {
  const store = await readHotkeyStoreAsync();
  return scanDeclaredHotkeys().map(d => {
    const ov = store.items[d.key] || {};
    const hasCombo = ov.combo !== undefined && ov.combo !== null;
    const hasEnabled = ov.enabled !== undefined && ov.enabled !== null;
    return {
      ...d,
      combo: hasCombo ? normCombo(ov.combo) : d.defaultCombo,
      enabled: hasEnabled ? !!ov.enabled : true,
      overridden: hasCombo || hasEnabled,
    };
  });
}

function suspendHotkeys() {
  for (const accel of hkRegistered.keys()) {
    try { globalShortcut.unregister(accel); } catch (e) { /* ignore */ }
  }
  hkRegistered.clear();
}

// applyHotkeys 按最终生效热键表重新注册全局键；返回逐条状态报告（async：读覆盖经内核 RPC）。
async function applyHotkeys() {
  suspendHotkeys();
  const items = await resolveHotkeys();
  const paletteCombo = normCombo(process.platform === "darwin" ? "Command+Shift+P" : "Control+K");
  hkReport = items.map(it => {
    const row = { key: it.key, pluginId: it.pluginId, id: it.id, name: it.name, combo: it.combo, status: "", error: "" };
    if (!it.enabled) { row.status = "disabled"; return row; }
    if (!it.combo) { row.status = "invalid"; row.error = "声明或覆盖的组合非法：" + (it.rawCombo || "(空)"); return row; }
    if (it.scope !== "global") { row.status = "app"; return row; }
    if (it.combo === paletteCombo) { row.status = "conflict"; row.error = "与命令面板热键冲突"; return row; }
    if (hkRegistered.has(it.combo)) {
      row.status = "conflict";
      row.error = "与 " + hkRegistered.get(it.combo).pluginId + "::" + hkRegistered.get(it.combo).id + " 重复";
      return row;
    }
    let ok = false;
    try { ok = globalShortcut.register(it.combo, () => fireHotkey(it)); }
    catch (e) { row.status = "invalid"; row.error = String((e && e.message) || e); return row; }
    if (!ok) { row.status = "conflict"; row.error = "已被系统或其他程序占用"; return row; }
    hkRegistered.set(it.combo, { pluginId: it.pluginId, id: it.id });
    row.status = "ok";
    return row;
  });
  return hkReport;
}

// hkWindows 热键呈现窗单例表：hotkey key → BrowserWindow（同热键再按只聚焦，不重复开窗）。
const hkWindows: Map<string, InstanceType<typeof BrowserWindow>> = new Map();

// fireHotkey 触发派发：优先 host 内置动作（action=host.*）；有呈现窗声明时，窗已开则聚焦，
// 否则触发 method 并按 window 声明通用开窗；无窗声明的仅调 method。
function fireHotkey(it) {
  try { if (win) win.webContents.send("hotkey:fired", { pluginId: it.pluginId, id: it.id, name: it.name, combo: it.combo }); } catch (e) {}
  const action = (it.action || "").trim();
  if (action === "host.palette.open" || action === "palette.open") {
    if (win) { win.show(); win.focus(); win.webContents.send("palette:open"); }
    return;
  }
  // translate::realtime（Alt+C）专用编排：复刻主面板按钮——未运行→框选区域→跳字幕窗；运行中→停止。
  // 其余热键仍走下面的通用「method + 呈现窗」路径。
  if (it.pluginId === "translate" && it.id === "realtime") { void fireTranslateRealtime(it); return; }
  const wd = it.hwin;
  if (wd) {
    if (wd.singleton) {
      const ex = hkWindows.get(it.key);
      if (ex && !ex.isDestroyed()) { ex.show(); ex.focus(); return; } // 再按 = 唤起已开窗
    }
    fireHotkeyMethod(it);   // 开窗同时触发（后端幂等；结果窗轮询 last_result）
    openHotkeyWindow(it, wd);
    return;
  }
  if (!it.method) { console.warn("[hotkey] 无 method/action，忽略", it.key); return; }
  fireHotkeyMethod(it);
}

function fireHotkeyMethod(it) {
  rpc("plugin.call", { pluginId: it.pluginId, method: it.method, params: it.params || {}, timeoutMs: it.timeoutMs || 15000 })
    .catch(e => {
      const msg = String((e && e.message) || e);
      console.error("[hotkey] 触发失败", it.key, msg);
      try { if (win) win.webContents.send("hotkey:error", { pluginId: it.pluginId, id: it.id, name: it.name, error: msg }); } catch (_) {}
    });
}

// openHotkeyWindow 按热键声明通用开窗：页面经内核 HTTP 提供，附 auth/kport 让子页自连 WS。
function openHotkeyWindow(it, wd) {
  if (!kernelAuth) return;
  const url = "http://127.0.0.1:" + kernelAuth.port + "/plugin/" + it.pluginId + "/" + wd.page;
  const full = url + "?auth=" + encodeURIComponent(kernelAuth.token) + "&kport=" + kernelAuth.port;
  const sub = spawnPluginWindow(full, { title: wd.title, width: wd.width, height: wd.height, frameless: wd.frameless });
  if (wd.singleton) {
    hkWindows.set(it.key, sub);
    sub.on("closed", () => { if (hkWindows.get(it.key) === sub) hkWindows.delete(it.key); });
  }
}

// ── translate::realtime（Alt+C）热键专用编排 ──────────────────────────
// 完全复刻 translate 插件页 runApp(key=screenSub) 的两阶段：
//   运行中 → 关字幕窗（sub_win beforeunload 自带 stop）+ 兜底显式 stop + 关常驻金框；
//   未运行 → pick_reset → 开 translate_overlay（interactive + persistent 金框）
//            → 等 pickerDone/Abort → 完成则按 hwin 打开 sub_win.html（其 boot 自启 screenSub 循环）。
const TRANSLATE_OVERLAY_ID = "translate_overlay";
let realtimeFlowBusy = false; // 防重入：编排进行中再按 Alt+C 直接忽略，避免叠加开窗

async function translateAppStatus() {
  const r = await rpc("plugin.call", { pluginId: "translate", method: "translate.app.status", params: {} }, 8000);
  return r && r.result !== undefined ? r.result : r;
}

// 等待框选结束：复刻 app.js waitPickFinish（300ms 轮询，默认 60s 上限）。
// 返回 'done' | 'abort' | 'timeout'。
function waitTranslatePickFinish(timeoutMs = 60000) {
  return new Promise((res) => {
    const start = Date.now();
    const t = setInterval(async () => {
      let a: any = {};
      try { a = await translateAppStatus(); } catch (e) { /* 单轮失败继续轮询 */ }
      if (a.pickerDone || a.pickerAbort || Date.now() - start > timeoutMs) {
        clearInterval(t);
        res(a.pickerDone ? "done" : (a.pickerAbort ? "abort" : "timeout"));
      }
    }, 300);
  });
}

function notifyHotkeyError(it: any, msg: string) {
  console.error("[hotkey] translate::realtime 编排失败:", msg);
  try { if (win) win.webContents.send("hotkey:error", { pluginId: it.pluginId, id: it.id, name: it.name, error: msg }); } catch (e) {}
}

async function fireTranslateRealtime(it: any) {
  if (realtimeFlowBusy) return;
  realtimeFlowBusy = true;
  try {
    let status: any = {};
    try { status = await translateAppStatus(); }
    catch (e: any) { notifyHotkeyError(it, "状态读取失败：" + ((e && e.message) || e)); return; }
    const existing = hkWindows.get(it.key);
    const running = !!status.subRunning || (existing && !existing.isDestroyed());
    if (running) {
      // 关字幕窗：sub_win beforeunload 自动 translate.app.stop + pick_abort；再兜底显式 stop。
      if (existing && !existing.isDestroyed()) existing.close();
      try {
        await rpc("plugin.call", { pluginId: "translate", method: "translate.app.stop", params: { key: "realtime" } }, 10000);
      } catch (e) { /* 窗内已 stop，忽略 */ }
      closeOverlayInternal(TRANSLATE_OVERLAY_ID); // 关常驻金框
      return;
    }
    // 未运行：清上一次 done/abort 残留 → 开持久金框（开窗即 pick 模态，后端此期间跳过采集）。
    try {
      await rpc("plugin.call", { pluginId: "translate", method: "translate.app.pick_reset", params: {} }, 8000);
    } catch (e) { /* 忽略 */ }
    const opened = await openOverlayInternal(TRANSLATE_OVERLAY_ID, { interactive: true, persistent: true });
    if (!opened || !opened.ok) { notifyHotkeyError(it, (opened && opened.error) || "打开选区失败"); return; }
    const pickSt = await waitTranslatePickFinish(60000);
    if (pickSt !== "done") {
      // 取消/超时：关金框并清状态（与 app.js runApp 的取消分支一致）。
      closeOverlayInternal(TRANSLATE_OVERLAY_ID);
      try {
        await rpc("plugin.call", { pluginId: "translate", method: "translate.app.pick_reset", params: {} }, 8000);
      } catch (e) { /* 忽略 */ }
      return;
    }
    // 完成：金框保留、sel.html 已自行切入 live 穿透态；打开字幕窗，其 boot() 幂等启动 screenSub 循环。
    openHotkeyWindow(it, it.hwin);
  } catch (e: any) {
    notifyHotkeyError(it, (e && e.message) || String(e));
  } finally {
    realtimeFlowBusy = false;
  }
}

ipcMain.handle("hotkeys:list", async () => ({ items: await resolveHotkeys(), report: await applyHotkeys(), store: "内核持有 state/hotkeys.json" }));
ipcMain.handle("hotkeys:normalize", async (e, { combo }: any = {}) => {
  const n = normCombo(combo);
  return { ok: !!n, combo: n };
});
ipcMain.handle("hotkeys:set", async (e, { key, combo, enabled }: any = {}) => {
  if (!key) return { ok: false, error: "缺少 key" };
  if (!scanDeclaredHotkeys().some(d => d.key === key)) return { ok: false, error: "热键未在任何插件中声明：" + key };
  const store = await readHotkeyStoreAsync();
  const cur = store.items[key] || {};
  if (combo !== undefined && combo !== null) {
    const norm = normCombo(combo);
    if (!norm) return { ok: false, error: "无效的按键组合（需修饰键+主键，如 Alt+X）：" + combo };
    cur.combo = norm;
  }
  if (enabled !== undefined && enabled !== null) cur.enabled = !!enabled;
  store.items[key] = cur;
  try { await writeHotkeyStoreAsync(store.items); } catch (err) { return { ok: false, error: String((err && err.message) || err) }; }
  return { ok: true, items: await resolveHotkeys(), report: await applyHotkeys() };
});
ipcMain.handle("hotkeys:reset", async (e, { key }: any = {}) => {
  const store = await readHotkeyStoreAsync();
  if (key) delete store.items[key]; else store.items = {};
  try { await writeHotkeyStoreAsync(store.items); } catch (err) { return { ok: false, error: String((err && err.message) || err) }; }
  return { ok: true, items: await resolveHotkeys(), report: await applyHotkeys() };
});
// 录制期间先注销全部已注册热键，否则按到已注册组合会直接触发动作、录不到键
ipcMain.handle("hotkeys:captureStart", async () => { suspendHotkeys(); return { ok: true }; });
ipcMain.handle("hotkeys:captureEnd", async () => ({ ok: true, items: await resolveHotkeys(), report: await applyHotkeys() }));

// 定位 uv：优先显式 OCTRUN_UV → 外置 bin/uv.exe → PATH（where uv）→ 常见安装目录。
function detectUv() {
  if (process.env.OCTRUN_UV) return process.env.OCTRUN_UV;
  try { const b = path.join(RUNTIME_ROOT, "bin", "uv.exe"); if (fs.existsSync(b)) return b; } catch (e) {}
  const { execSync } = require("child_process");
  try {
    const hit = execSync("where uv", { encoding: "utf8" }).split(/\r?\n/)[0].trim();
    if (hit) return hit;
  } catch (e) { /* 不在 PATH，继续查安装目录 */ }
  const home = os.homedir();
  for (const c of [path.join(home, ".local", "bin", "uv.exe"),
                   path.join(home, ".cargo", "bin", "uv.exe")]) {
    try { if (fs.existsSync(c)) return c; } catch (e) {}
  }
  return null;
}

function startKernel(): Promise<any> {
  return new Promise<any>((resolve, reject) => {
    // /c 让 Go 输出 stdio 原始（Windows 下避免 Go 检测 tty 出问题）
    // 注入 OCTRUN_UV：Windows 下 uv 常不在 PATH，内核依赖它解析托管 Python。
    const uv = detectUv();
    const env = uv ? Object.assign({}, process.env, { OCTRUN_UV: uv }) : process.env;
    kernelProc = spawn(KERNEL_EXE, [], { stdio: ["pipe", "pipe", "pipe"], windowsHide: true, env });

    let buf = "";
    kernelProc.stdout.on("data", (d) => {
      buf += d.toString();
      const nl = buf.indexOf("\n");
      if (nl < 0) return;
      const line = buf.slice(0, nl).trim();
      try {
        const { auth } = JSON.parse(line);
        resolve(auth);
      } catch (e) {
        reject(new Error("内核 auth 行解析失败: " + line));
      }
    });
    kernelProc.stderr.on("data", (d) => console.error("[kernel]", d.toString().trim()));
    kernelProc.on("exit", (code) => {
      if (buf.indexOf("\n") < 0) reject(new Error(`内核提前退出 code=${code}`));
    });
    // 内核在 StartAll 期间会做依赖就绪探测（亚秒~15s 上限），auth 只在其后打印；
    // 5s 过短会误判。60s 覆盖首次依赖探测/下载，避免启动偶发超时。
    setTimeout(() => reject(new Error("等待内核 auth 超时")), 60000);
  });
}

function connect(auth: any): Promise<any> {
  return new Promise<any>((resolve, reject) => {
    ws = new WebSocket(`ws://127.0.0.1:${auth.port}`);
    ws.on("open", () => {
      const id = Date.now();
      ws.send(JSON.stringify({ v: 1, jsonrpc: "2.0", id, method: "kernel.hello",
                               params: { client: "octplugin-host", token: auth.token } }));
      ws.on("message", (m) => {
        const msg = JSON.parse(m.toString());
        if (msg.id === id) resolve(msg);
        else {
          const p = (msg && msg.params) || {};
          // 区分器：宿主一收到插件事件即向任一 overlay 投一可见提示，用于判定断点(无需终端)。
          if (p.source) {
            const anyOv = overlayWindows.size ? overlayWindows.values().next().value : undefined;
            if (anyOv && !anyOv.isDestroyed()) {
              try { anyOv.webContents.send("overlay:echo", JSON.stringify({ source: p.source, type: msg.params.type })); } catch (e) {}
            }
          }
          const ov = p.source ? overlayRoute.get(p.source) : undefined;
          if (ov && !ov.isDestroyed()) {
            try { ov.webContents.send("overlay:event", JSON.stringify(msg)); } catch (e) {}
          } else if (p.source) {
            console.log("[overlay:miss]", p.source, msg.method, "route=", !!overlayRoute.get(p.source));
          }
          // 判活：主窗关闭后内核广播仍可能到达，直接 send 会抛 Object has been destroyed
          if (win && !win.isDestroyed()) win.webContents.send("kernel:event", JSON.stringify(msg));
        }
      });
    });
    ws.on("error", reject);
  });
}

function rpc(method, params, timeoutMs = 15000): Promise<any> {
  return new Promise<any>((resolve, reject) => {
    if (!ws) {
      reject(new Error("内核未连接（ws 为空）— 请检查 kerneld.exe 是否存在并成功启动"));
      return;
    }
    const id = ++seq;
    ws.send(JSON.stringify({ v: 1, jsonrpc: "2.0", id, method, params }));
    const timer = setTimeout(() => { ws.off("message", onMsg); reject(new Error("RPC 超时: " + method)); }, timeoutMs || 15000);
    const onMsg = (m) => {
      const msg = JSON.parse(m.toString());
      if (msg.id !== id) return;
      ws.off("message", onMsg);
      clearTimeout(timer);
      if (msg.error) {
        // §19 派生错误码：附上诊断名（EDiagByCode[code]）便于宿主与用户定位。
        const diag = msg.error.data && msg.error.data.diag ? msg.error.data.diag
                   : (msg.error.code !== undefined ? (EDiagByCode[msg.error.code] || "") : "");
        const tag = diag ? "[" + diag + "] " : "";
        reject(new Error(tag + msg.error.message + " " + JSON.stringify(msg.error.data || "")));
      } else resolve(msg.result);
    };
    ws.on("message", onMsg);
  });
}

// §27：单实例。Windows「打开方式」以新进程拉起 → 无锁则退出；有锁则转发文件路径。
const gotSingleLock = app.requestSingleInstanceLock();
if (!gotSingleLock) {
  app.quit();
} else {
  app.on("second-instance", (_e, argv) => {
    const f = extractOpenFile(argv.slice(1));
    if (f) handleOpenFile(f); else showMain();
  });
  app.whenReady().then(() => {
    createWindow();
    setupTray();
    registerShortcuts();
    registerFileAssoc(); // §27：静默注册「打开方式」，失败不阻断启动
    const f0 = extractOpenFile(process.argv.slice(1));
    if (f0) handleOpenFile(f0); // 首次启动即带文件（应用未运行时右键打开）
  });
}

// 阶段D·全局热键（FR-9）：Ctrl+K / Ctrl+Shift+P 打开命令面板，带冲突检测兜底
function registerShortcuts() {
  const openPalette = () => {
    if (!win) return;
    win.show(); win.focus();
    win.webContents.send("palette:open");
  };
  const combo = process.platform === "darwin" ? "Command+Shift+P" : "Control+K";
  const registered = globalShortcut.register(combo, openPalette);
  if (!registered) {
    // 冲突兜底：改用窗口内 before-input-event 捕获（不冒充系统级全局键）
    console.warn(`[host] globalShortcut ${combo} 被占用（冲突），回退窗口内热键`);
    win.webContents.on("before-input-event", (event, input) => {
      if (input.type !== "keyDown") return;
      const k = (input.key || "").toUpperCase();
      if ((input.control || input.meta) && k === "K") { event.preventDefault(); openPalette(); }
    });
  }
}

let quittingAfterProxyShutdown = false;
let proxyShutdownStarted = false;

app.on("before-quit", (event) => {
  if (quittingAfterProxyShutdown) return;
  event.preventDefault();
  if (proxyShutdownStarted) return;
  proxyShutdownStarted = true;

  (async () => {
    try {
      if (kernelProc && ws) {
        const listing = await rpc("plugin.list", {}, 2500);
        const proxy = (listing && listing.plugins || []).find((item: any) => item.pluginId === "proxy");
        if (proxy && proxy.state === "RUNNING") {
          await rpc("plugin.call", {
            pluginId: "proxy",
            method: "shutdown",
            params: {},
            timeoutMs: 5000,
          }, 6000);
        }
      }
    } catch (err: any) {
      console.warn("[host] proxy graceful shutdown failed:", String((err && err.message) || err));
    } finally {
      quittingAfterProxyShutdown = true;
      if (kernelProc) {
        kernelProc.kill();
        kernelProc = null;
      }
      app.quit();
    }
  })();
});

app.on("will-quit", () => {
  globalShortcut.unregisterAll();
  overlayWindows.forEach((w) => { if (w && !w.isDestroyed()) w.destroy(); });
  overlayWindows.clear();
  overlayRoute.clear();
});

app.on("window-all-closed", () => {
  app.quit();
});