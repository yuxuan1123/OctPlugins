// OCTplugin 宿主主进程（MVP）：
//   1) 拉起 Go 内核 daemon（./kernel/kerneld.exe）
//   2) 从内核 stdout 读首个 auth 行 {port, token}
//   3) WebSocket 连接 localhost:port，首条消息发带 token 的 kernel.hello
//   4) 调 plugin.list → 结果在窗口显示。
export {};

const { app, BrowserWindow, dialog, ipcMain, globalShortcut, Tray, Menu, nativeImage } = require("electron");
const { spawn } = require("child_process");
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

// 给宿主主进程一个可辨认的标题（任务管理器/进程列表里更容易区分）
process.title = "OCTplugin · 墨韵工作台";

// 插件请求以独立窗口打开页面（如 md 编辑器）。宿主把内核地址放查询参数，
// 子页 app.js 见 ?mdEditor=1&kport=&auth= 时自连内核 WS。
ipcMain.handle("win:openPluginWindow", async (e, opts) => {
  const url = opts && opts.url;
  if (!url || !kernelAuth) return { ok: false, error: !kernelAuth ? "内核未就绪" : "无 url" };
  const sep = url.includes("?") ? "&" : "?";
  const full = url + sep + "auth=" + encodeURIComponent(kernelAuth.token) + "&kport=" + kernelAuth.port;
  const sub = new BrowserWindow({
    width: opts.width || 960, height: opts.height || 720,
    title: opts.title || "插件窗口", icon: APP_LOGO,
    autoHideMenuBar: true,
    webPreferences: { nodeIntegration: false, contextIsolation: true },
  });
  sub.loadURL(full);
  return { ok: true };
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
const FIRST_PARTY_PLUGINS = []; // 一方插件自动放权名单（本项目内置 home/demo_web，无需预授权）

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
    win.webContents.send("kernel:ready", {
      list,
      kernelBase: `http://127.0.0.1:${auth.port}/`,
      resBase: `http://127.0.0.1:${auth.port}/res/`,
      port: auth.port, token: auth.token,
    });
  } catch (e) {
    console.error("启动失败:", e);
    win.webContents.send("kernel:error", String(e));
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
      out.push({
        key: pid + "::" + hk.id, pluginId: pid, pluginName: mf.name || pid, id: hk.id,
        name: hk.name || hk.id, desc: hk.desc || "",
        rawCombo, defaultCombo: normCombo(rawCombo),
        scope: hk.scope === "app" ? "app" : "global",
        method: hk.method || "", action: hk.action || "",
        params: hk.params || {}, timeoutMs: hk.timeoutMs || 0,
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

// fireHotkey 触发派发：优先 host 内置动作（action=host.*），否则经内核 plugin.call 调插件方法。
function fireHotkey(it) {
  try { if (win) win.webContents.send("hotkey:fired", { pluginId: it.pluginId, id: it.id, name: it.name, combo: it.combo }); } catch (e) {}
  const action = (it.action || "").trim();
  if (action === "host.palette.open" || action === "palette.open") {
    if (win) { win.show(); win.focus(); win.webContents.send("palette:open"); }
    return;
  }
  if (!it.method) { console.warn("[hotkey] 无 method/action，忽略", it.key); return; }
  rpc("plugin.call", { pluginId: it.pluginId, method: it.method, params: it.params || {}, timeoutMs: it.timeoutMs || 15000 })
    .catch(e => {
      const msg = String((e && e.message) || e);
      console.error("[hotkey] 触发失败", it.key, msg);
      try { if (win) win.webContents.send("hotkey:error", { pluginId: it.pluginId, id: it.id, name: it.name, error: msg }); } catch (_) {}
    });
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
        else win.webContents.send("kernel:event", JSON.stringify(msg));
      });
    });
    ws.on("error", reject);
  });
}

function rpc(method, params, timeoutMs = 15000): Promise<any> {
  return new Promise<any>((resolve, reject) => {
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

app.whenReady().then(() => {
  createWindow();
  setupTray();
  registerShortcuts();
});

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

app.on("will-quit", () => { globalShortcut.unregisterAll(); });

app.on("window-all-closed", () => {
  if (kernelProc) kernelProc.kill();
  app.quit();
});