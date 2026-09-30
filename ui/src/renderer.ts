
    // 本文件作为经典 <script>（非模块）加载，宿主页面经 nodeIntegration 提供 require/ipcRenderer。
// renderer.js 由 tsc(module=commonjs) 编译；此处不能出现任何 import/export——
// 否则 tsc 会生成 `Object.defineProperty(exports,…)`，而经典脚本上下文无 CommonJS exports，
// 会抛 `exports is not defined` 使整段脚本在第 2 行即中断（此前“添加插件”等全部按钮失灵）。
// 顶层标识符不与 main.ts 冲突：main.ts 已用 `export {}` 隔离为模块作用域，其 rpc 等不进入全局。
// 由 index.html 内联脚本迁移（字节级一致），tsc 转型到 build/renderer.js
const { ipcRenderer } = require("electron");
// 动态 DOM 边界：getElementById 返回 HTMLElement|null，Query 结果无 style/dataset 属性。
// 统一以 any 承接（Web/DOM 运行期类型不可静态穷举，属对外动态边界）。
const $: (id: string) => any = (id) => document.getElementById(id);
const setStatus = (t: string, c: string) => { $("status").textContent = t; $("status").className = "status " + c; };
const rpc: (m: string, p?: any, t?: number) => Promise<any> = (m, p, t) => ipcRenderer.invoke("rpc", m, p, t) as Promise<any>;
const $$: (sel: string) => any = (sel) => document.querySelectorAll(sel);

    // ── 设置子窗口模式：读取 ?view=settings&sec=…，只显示对应那张设置卡片 ──
    // 说明：index.html 不做 auth/kport 自连内核；渲染脚本始终走 ipcRenderer.invoke("rpc") 由宿主主进程
    // 复用内核连接。子窗口加载同一份 index.html，故这里仅需 URL 中的 view/sec 决定显示哪张表单。
    const QP = new URLSearchParams(location.search || "");
    const IS_SETTINGS = QP.get("view") === "settings";
    if (IS_SETTINGS) {
      const SEC = QP.get("sec");
      document.body.classList.add("settings-window");
      const names = { A: "依赖源设置", B: "启动与资源·全局策略", C: "启动与资源·单插件策略", D: "插件排序·默认页", E: "插件列表", F: "插件内部设置", G: "插件依赖（隔离环境）", H: "已安装库及版本", I: "共享函数", K: "外部地址与内存设置", L: "热键（快捷键）" };
      const titleEl = document.getElementById("settTitle");
      if (titleEl) titleEl.textContent = "设置 · " + (names[SEC] || SEC || "");
      // 只亮出当前 sec 的卡片，其余表单隐藏（DOM 全部保留，供各加载函数引用不报错）
      document.querySelectorAll("#settingsForms > .card").forEach((c: any) => {
        c.style.display = (c.dataset.sec === SEC) ? "" : "none";
      });
      // 关闭按钮：优先让主进程关掉本窗口，失败再退回 window.close()
      const closeBtn = document.getElementById("btnSettingsClose");
      if (closeBtn) closeBtn.onclick = async () => {
        try { await ipcRenderer.invoke("win:closeSelf"); } catch (e) { window.close(); }
      };
      // 主窗的 kernel:ready 只发给主窗，子窗口收不到；改用宿主 rpc 桥自行填充各设置表单
      queueMicrotask(() => {
        if (typeof loadDepCfg === "function") loadDepCfg();
        if (typeof refreshOrderSettings === "function") refreshOrderSettings();
        if (typeof refreshMgmt === "function") refreshMgmt();
        if (typeof wirePluginPicker === "function") wirePluginPicker();
        if (typeof loadPerPluginList === "function") loadPerPluginList();
        if (typeof refreshFns === "function") refreshFns();
        if (typeof loadResources === "function") loadResources();
        if (typeof refreshHotkeys === "function") refreshHotkeys();
      });
    }
    // 设置列表：点「设置」在主窗口外打开独立设置子窗口
    document.querySelectorAll("#settingsList .sett-btn").forEach((b: any) => {
      b.onclick = () => ipcRenderer.invoke("win:openSettings", { sec: b.dataset.sec });
    });

    // ── 自绘窗口控制（frame:false 无边框） ──
    $("btnMin").onclick = () => ipcRenderer.send("win:minimize");
    $("btnMax").onclick = () => ipcRenderer.send("win:maximize");
    $("btnClose").onclick = () => ipcRenderer.send("win:close");
    $("btnTray").onclick = () => ipcRenderer.send("win:tray");
    // 点击 logo 隐藏/展开左侧栏
    $("logo").onclick = () => document.body.classList.toggle("side-collapsed");
    ipcRenderer.on("win:state", (e, s) => {
      const b = $("btnMax");
      const maxed = s === "max";
      b.classList.toggle("restore", maxed);
      b.title = maxed ? "还原" : "最大化";
    });

    // ── 阶段G · 插件 iframe 装配（web 插件自动进侧栏） ──
    let KERNEL_BASE = ""; let OCT_CONN = { port: null, token: "" };
    const mounted = new Set();
    // paintDot 按内核真实 State + Disabled 标志给侧栏按钮着色（当前页的红由 switchPanel 的 active 类负责，这里不参与）。
    function paintDot(btn, state, disabled = false) {
      btn.classList.toggle("running", state === "RUNNING" || state === "RUNNING_EXT");
      btn.classList.toggle("starting", state === "STARTING");
      btn.classList.toggle("disabled", !!disabled);
    }
    // 点击懒启动后的兜底：若插件状态事件存在转发延迟/丢失，轮询 plugin.list 用内核真实状态收敛圆点。
    function pollState(btn, pid, tries) {
      const n = tries || 0;
      if (!btn.isConnected || n >= 8) { if (btn.isConnected) paintDot(btn, "REGISTERED"); return; }
      rpc("plugin.list", {}).then(r => {
        const p = (r.plugins || []).find(x => x.pluginId === pid);
        const s = p && p.state;
        if (s === "RUNNING" || s === "RUNNING_EXT") { paintDot(btn, s); return; }
        setTimeout(() => pollState(btn, pid, n + 1), 600);
      }).catch(() => setTimeout(() => pollState(btn, pid, n + 1), 600));
    }
    // ── 侧栏排序 + 默认页（宿主级 UI 设置，落 user-settings.json -> ui，§17.2 B；去 localStorage） ──
    // uiPrefs 为内核 user-settings.json 的 ui 节镜像缓存（同步读友好），经 ui.getSettings/ui.setSettings 同步。
    let uiPrefs: { sidebarOrder: string[]; defaultPage: string } = { sidebarOrder: [], defaultPage: "home" };
    async function loadUIPrefs() {
      try {
        const r = await rpc("ui.getSettings", {});
        const u = (r && r.ui) || {};
        if (Array.isArray(u.sidebarOrder)) uiPrefs.sidebarOrder = u.sidebarOrder;
        if (typeof u.defaultPage === "string") uiPrefs.defaultPage = u.defaultPage;
      } catch { /* 内核未就绪时保持默认 */ }
      return uiPrefs;
    }
    async function saveUIPrefs(delta: { sidebarOrder?: string[]; defaultPage?: string }) {
      uiPrefs = { ...uiPrefs, ...delta }; // 乐观更新：先应用
      try { await rpc("ui.setSettings", { ui: uiPrefs }); } catch { /* 落盘失败不阻断本次应用 */ }
      return uiPrefs;
    }
    function defaultPageId() { return uiPrefs.defaultPage || "home"; }
    // 已知顺序区优先（home 始终在首个未命名的可指定位置也算靠前），未命名的按原序追加。
    function orderedPlugins(plugins) {
      const order = uiPrefs.sidebarOrder || [];
      const rank = {};
      order.forEach((id, i) => rank[id] = i);
      let next = order.length, homeRank = Infinity;
      const r = p => (p.pluginId in rank) ? rank[p.pluginId]
                    : (p.pluginId === "home" ? (homeRank === Infinity ? (homeRank = order.length - 0.5) : homeRank) : next++);
      return [...plugins].sort((a, b) => r(a) - r(b));
    }
    // 启动后打开默认插件页（未运行则先拉起进程，逻辑同侧栏点击）。
    function openDefaultPlugin() {
      const pid = defaultPageId();
      const btn = document.querySelector('button[data-plugin-id="' + pid + '"]');
      if (btn) {
        if (!btn.classList.contains("disabled") && !btn.classList.contains("running")) {
          paintDot(btn, "STARTING");
          rpc("plugin.start", { pluginId: pid }, 120000).then(() => {}).catch(() => {});
          pollState(btn, pid, 0);
        }
        switchPanel("plugin." + pid);
        return true;
      }
      return false;
    }
    // ── 侧栏排序 + 默认页 ──
    function renderPlugins(plugins) {
      const nav = document.querySelector("nav.side");
      const border = document.getElementById("pluginArea");
      // 侧栏/主页只展示普通插件（kind!==tool 且 kind!==overlay）；工具/overlay 仅在管理面板列出。
      plugins = (plugins || []).filter(p => p && p.kind !== "tool" && p.kind !== "overlay");
      // 侧栏顺序以用户设置为主（user-settings.json ui.sidebarOrder，§17.2 B）；未配置时 home 置首、其余维持原序。
      plugins = orderedPlugins(plugins);
      // 清空上一次动态区（保留 设置/调试 的“工作区”与新组）
      $$(".plugin-nav").forEach(n => n.remove());
      $$(".plugin-panel").forEach(p => p.remove());
      const web = plugins.filter(p => p.ui && p.ui.type === "web");
      if (web.length) {
        const sep = document.createElement("div");
        sep.className = "nav-title plugin-nav"; sep.textContent = "插件";
        sep.style.cssText = "margin-top:14px;";
        const anchor = document.querySelector("nav.side button[data-panel]");
        nav.insertBefore(sep, anchor.nextSibling ? anchor.nextSibling : null);
        // 每次插到上一个按钮之后，保证 DOM 顺序与数组顺序一致（否则会反转）。
        let prev: HTMLElement = sep;
        web.forEach((p, i) => {
          const pid = p.pluginId;
          const btn = document.createElement("button");
          btn.className = "side-btn plugin-nav"; btn.dataset.panel = "plugin." + pid;
          // 圆点仅由内核真实 State 驱动：RUNNING→黑；disabled→灰+划线(已停)；其余(REGISTERED/IDLE/STOPPED)→灰。
          btn.dataset.pluginId = pid;
          paintDot(btn, p.state, p.disabled);
          btn.innerHTML = `<span class="dot"></span><span class="name">${(p.name || pid).slice(0, 6)}</span>`;
          btn.onclick = () => {
            // 即点即切：立即 switchPanel（加载 iframe），不等插件冷启动；
            // 进程在后台拉起，插件 UI 首次 plugin.call 会触发内核 GetOrStart 兜底，就绪后自行渲染。
            switchPanel("plugin." + pid);
            if (!btn.classList.contains("disabled") && !btn.classList.contains("running")) {
              paintDot(btn, "STARTING");
              rpc("plugin.start", { pluginId: pid }, 120000)
                .then(() => pollState(btn, pid, 0))   // 兜底：事件延迟/丢失时用内核真实状态收敛
                .catch((e) => { console.warn("[plugin] start 失败", pid, e); pollState(btn, pid, 0); });
            }
          };
          nav.insertBefore(btn, prev.nextSibling); prev = btn;
          // 面板
          const sec = document.createElement("section");
          sec.className = "panel plugin-panel"; sec.id = "panel-plugin." + pid;
          const frame = document.createElement("iframe");
          frame.dataset.plugin = pid;
          // 首展开前不设 src（存于 data-src），由 showPanel→ensurePluginFrame 首次打开时才加载并拉起对应进程。
          // 若启动时就把每个 web 插件 UI 都载入，其页面 JS 会在加载后自调 plugin.call → 触发内核 GetOrStart，
          // 从而把 load_mode=lazy 的插件（translation/merge/conversion…）在无任何点击时全部拉起。
          // 打开过一次后 src 保留，之后侧栏切换即时、无空白。
          frame.dataset.src = KERNEL_BASE + "plugin/" + pid + "/" + (p.ui.entry || "ui/index.html");
          frame.setAttribute("style", "width:100%;height:100%;border:none;border-radius:14px;background:transparent;display:block;");
          sec.appendChild(frame);
          frame.addEventListener("load", () => mount(frame, sec)); // 仅握手，不改色（颜色只信内核 State）
          frame.addEventListener("error", () => {
            const ph = document.createElement("div");
            ph.style.cssText = "position:absolute;inset:0;display:flex;align-items:center;justify-content:center;color:#b23a30;font-size:13px;background:#f3ecdd;";
            ph.textContent = `加载失败并无法诊断（iframe error）：${frame.src}`;
            sec.appendChild(ph);
          });
          border.appendChild(sec); // 关键：把插件面板挂进 #pluginArea（此前丢失导致主区空白）
        });
      }
    }
    // iframe 加载完成后，把内核连接信息 postMessage 给插件页，插件页据此直连内核 WS。
    // 插件面板填满由 CSS(.panel.plugin-panel.active 绝对定位)负责，这里只做握手。
    function mount(frame, sec) {
      if (!frame.contentWindow) return;
      const id = frame.dataset.plugin;
      // 关键：iframe 首建时不设 src，浏览器先加载 about:blank 并触发 load；此刻 src 为空，
      // 若握手/标记会把真实插件页（随后由 ensurePluginFrame 设 src 载入）的二次 load 误判为已处理而跳过握手，
      // 导致插件页面收不到 oct.hello → 不连 WS → 内容空白。因此空白页 load 直接忽略，等真实页 load 再握手。
      if (!frame.src || frame.src === "about:blank") return;
      try {
        if (mounted.has(id)) return; mounted.add(id);
        frame.contentWindow.postMessage({ type: "oct.hello", port: OCT_CONN.port, token: OCT_CONN.token, pluginId: id }, "*");
        // 加载后探测 iframe 内是否真有内容（区分“空白”是 iframe 空还是页面空）。
        // 懒加载冷启动时插件进程拉起需数秒，UI 靠 JS 渲染、body 短暂为空属正常。
        // 此处仅作诊断（console 提示），绝不遮罩面板，否则会盖住正在加载的真实 UI。
        setTimeout(() => {
          try {
            const doc = frame.contentDocument;
            if (!doc || !doc.body) return;
            if ((doc.body.innerHTML || "").length < 10) {
              console.warn(`[plugin] ${id} 内容仍为空（body长度=0），src=${frame.src}`);
            }
          } catch (e) { /* 跨域不可读，忽略 */ }
        }, 8000);
      } catch (e) { console.error("[plugin] 握手失败", id, e); }
    }
    // 插件 iframe 请求“以独立窗口打开页面”（如 md 编辑器）：组装内核地址并交主进程开窗。
    // 子页 app.js 读取查询参数 auth/kport 自连内核。仅接受本内核 origin 的消息。
    window.addEventListener("message", (ev) => {
      const d = ev.data || {};
      if (d.type === "oct.openPluginWindow") {
        if (!d.path) return;
        if (ev.origin && KERNEL_BASE && !("" + ev.origin).includes("127.0.0.1")) return;
        const url = KERNEL_BASE + String(d.path || "").replace(/^\//, "");
        ipcRenderer.invoke("win:openPluginWindow", {
          url, title: d.title, width: d.width, height: d.height, frameless: !!d.frameless,
        });
        return;
      }
      // 插件请求原生文件/目录对话框：action = 'openFile'|'pickDir'，带 reqId 回调
      if (d.type === "oct.dialog" && d.action) {
        if (ev.origin && KERNEL_BASE && !("" + ev.origin).includes("127.0.0.1")) return;
        (async () => {
          let r: any = { path: "", canceled: true };
          try {
            if (d.action === "pickFile") {
              r = await ipcRenderer.invoke("dialog:pickFile", d.filters || []);
            } else {
              r = await ipcRenderer.invoke("dialog:pickDir");
            }
          } catch (e) { r = { path: "", canceled: true, error: String(e) }; }
          try { ev.source.postMessage({ type: "oct:dialog:result", reqId: d.reqId, result: r }, "*" as any); }
          catch (e) { /* source 不可达则忽略 */ }
        })();
        return;
      }
      // 通用工具(tool)桥：插件 iframe 经宿主 main 调任意 tool 进程（无量硬编码，toolId=manifest.id）。
      // 工具事件由插件侧自己的 WS 订阅内核事件频道获得，宿主不中转具体事件负载。
      if (d.type === "oct.tool.invoke") {
        if (ev.origin && KERNEL_BASE && !("" + ev.origin).includes("127.0.0.1")) return;
        const p: any = d.params || {};
        ipcRenderer.invoke("tool:invoke", { toolId: p.toolId, method: p.method, params: p.params })
          .then((r: any) => {
            try { ev.source.postMessage({ type: "oct:tool:result", reqId: d.reqId || 0, ok: !!(r && r.ok), result: (r && r.result) || null, error: (r && r.error) || "" }, "*" as any); }
            catch (e) { /* 忽略 */ }
          });
        return;
      }
      // 插件登记：它想接收来自某 toolId 的事件（宿主按 source 转发给该 iframe）。
      if (d.type === "oct.tool.subscribe") {
        const pid = pidOfFrame(ev.source);
        if (pid && d.toolId) {
          if (!toolSubs.has(d.toolId)) toolSubs.set(d.toolId, new Set());
          toolSubs.get(d.toolId)!.add(pid);
        }
        return;
      }
      // 通用剪贴板写入：插件 iframe 在取色期间非焦点文档，navigator.clipboard 会被静默拒绝，转主进程写。
      if (d.type === "oct.clipboard.write") {
        if (typeof d.text === "string") ipcRenderer.invoke("clipboard:write", { text: d.text });
        return;
      }
      // 通用 overlay：请求宿主开/关某 kind:"overlay" 插件为其全屏透明鼠标穿透窗。
      if (d.type === "oct.overlay") {
        if (ev.origin && KERNEL_BASE && !("" + ev.origin).includes("127.0.0.1")) return;
        const pluginId = d.pluginId, toolId = d.toolId;
        if (d.action === "open" && pluginId) ipcRenderer.invoke("overlay:open", { pluginId, toolId });
        else if (d.action === "close" && pluginId) ipcRenderer.invoke("overlay:close", { pluginId });
        return;
      }
    });
    // toolId → 订阅它的插件 iframe id 集合（宿主据此把 tool 事件转发给对应插件页）。
    const toolSubs: Map<string, Set<string>> = new Map();
    function pidOfFrame(w: any): string {
      const frames: any = document.querySelectorAll("iframe[data-plugin]");
      for (const f of frames) if (f.contentWindow === w) return f.getAttribute("data-plugin");
      return "";
    }
    // 向指定插件 iframe 转发事件（contentWindow.postMessage 由其打开；targetOrigin '*' 保持与握手一致）
    function forwardToPlugin(pid: string, obj: any) {
      try {
        const fr: any = document.querySelector(`iframe[data-plugin="${pid}"]`);
        if (fr && fr.contentWindow) fr.contentWindow.postMessage(obj, "*" as any);
      } catch (e) { /* 忽略 */ }
    }

    // ── 阶段1 · 「添加插件」管理面板 ──
    const PLUGIN_ROLE = {}; // pluginId -> 'core'|'others'（用于全局预设时区分核心/其余）
    async function refreshMgmt() {
      try {
        const r = await rpc("plugin.list", {});
        // 插件管理面板（pluginMgmt）仍列出全部条目（含 kind:"tool" 工具，供管理）；
        // 工具化展示见「工具」Tab（refreshTools）。侧栏/主页已由 renderPlugins 过滤 tool。
        const list = r.plugins || [];
        $("mgmtList").innerHTML = list.length ? list.map(p =>
          `<div class="perm" style="flex-wrap:wrap"><span>
            <input type="checkbox" data-pid="${p.pluginId}" data-type="${p.type}" data-ui="${p.ui && p.ui.type || ''}">
            <span style="color:var(--ink)">${p.name || p.pluginId}</span>
            ${p.kind === "tool" ? `<span class="tool-badge">工具</span>` : ""}
            <span style="color:var(--ink-faint);font-size:11px;margin-left:8px">${p.type}${p.ui && p.ui.type === 'web' ? ' · 网页UI' : ''}</span>
          </span>
          <button class="mini" data-rm="${p.pluginId}">移除</button>
          <button class="mini" data-lc="${p.pluginId}">启动·策略</button></div>
          <div class="lc-edit" id="lceedit-${p.pluginId}" style="display:none;padding:6px 12px 14px;"></div>`).join("")
          : "（暂无插件）";
        $("mgmtList").querySelectorAll("[data-rm]").forEach(b => b.onclick = async (e) => {
          e.stopPropagation();
          if (!confirm(`确定移除插件 ${b.dataset.rm}？（将删除其目录）`)) return;
          try { await rpc("plugin.remove", { pluginId: b.dataset.rm }); $("mgmtLog").textContent = "已移除 " + b.dataset.rm; refreshMgmt(); }
          catch (err) { $("mgmtLog").textContent = "移除失败：" + err.message; }
        });
        $("mgmtList").querySelectorAll("[data-lc]").forEach(b => b.onclick = () => toggleLifecycleEditor(b.dataset.lc));
        // 载入全部插件生命周期，用于引用标记
        for (const p of list) PLUGIN_ROLE[p.pluginId] = r._core && r._core.includes(p.pluginId) ? 'core' : 'others';
      } catch (e) { $("mgmtList").textContent = "读取失败：" + e.message; }
    }
    // ── 「工具」管理面板（kind:"tool" 无 UI 进程实体，不进侧栏/主页，仅在此列出/导入/移除） ──
    async function refreshTools() {
      try {
        const r = await rpc("plugin.list", {});
        const tools = (r.plugins || []).filter(p => p && p.kind === "tool");
        $("toolMgmtList").innerHTML = tools.length ? tools.map(p =>
          `<div class="perm" style="flex-wrap:wrap"><span>
            <input type="checkbox" data-pid="${p.pluginId}" data-tool="1">
            <span style="color:var(--ink)">${p.name || p.pluginId}</span>
            <span class="tool-badge">工具</span>
            <span style="color:var(--ink-faint);font-size:11px;margin-left:6px">${p.type} · ${p.state || ''}</span>
          </span>
          <button class="mini" data-rmtool="${p.pluginId}">移除</button></div>`).join("")
          : "（暂无工具）。可点击右方「选择工具源目录并导入」导入 kind:\"tool\" 的进程工具。";
        $("toolMgmtList").querySelectorAll("[data-rmtool]").forEach(b => b.onclick = async (e) => {
          e.stopPropagation();
          if (!confirm(`确定移除工具 ${b.dataset.rmtool}？（将删除其目录）`)) return;
          try { await rpc("plugin.remove", { pluginId: b.dataset.rmtool }); $("toolMgmtLog").textContent = "已移除工具 " + b.dataset.rmtool; refreshTools(); }
          catch (err) { $("toolMgmtLog").textContent = "移除失败：" + err.message; }
        });
      } catch (e) { $("toolMgmtList").textContent = "读取失败：" + e.message; }
    }
    // 添加插件面板 Tab 切换（插件 / 工具）
    function switchAddTab(tab) {
      const isTool = tab === "tools";
      $("tabPlugins").classList.toggle("active", !isTool);
      $("tabTools").classList.toggle("active", isTool);
      $("tabPluginsBody").classList.toggle("add-tools-hidden", isTool);
      $("tabToolsBody").classList.toggle("add-tools-hidden", !isTool);
      if (isTool) refreshTools();
    }
    // 单插件生命周期编辑器：拉取视图渲染表单，保存调 plugin.updateSettings
    // 挂在指定容器 box 下；id 唯一前缀用于多容器（管理列表 / 设置列表）复用。
    async function toggleLifecycleEditor(id, box = null, force = false) {
      box = box || $("lceedit-" + id);
      if (!force) {
        const show = box.style.display === "none";
        box.style.display = show ? "block" : "none";
        if (!show) return;
      }
      box.style.display = "block";
      box.innerHTML = "读取中…";
      let view;
      try { view = await rpc("plugin.getLifecycle", { pluginId: id }); }
      catch (e) { box.innerHTML = `<span class="risk">读取失败：${e.message}</span>`; return; }
      const eff = view.effective || {};
      const st = eff.startup || {}, hb = eff.heartbeat || {}, rc = eff.recycle || {};
      const lim = eff.limits || {}, res = eff.resilience || {};
      const P = "lc-" + (box.id || "x") + "-" + id;
      const curMode = eff.load_mode || "prewarm";
      const grp = (t) => `<div class="lc-grp">${t}</div>`;
      box.innerHTML = `
        ${grp("启动")}
        <div class="lc-grid">
          <div class="lc-num"><span>加载模式</span><select class="lc-sel" id="${P}-mode">${["always","prewarm","lazy","disabled"].map(m=>`<option value="${m}" ${curMode===m?"selected":""}>${{always:"常驻 always",prewarm:"预热 prewarm",lazy:"按需 lazy",disabled:"禁用 disabled"}[m]}</option>`).join("")}</select></div>
          <div class="lc-num"><span>预热池大小</span><input id="${P}-prewarm" type="number" min="0" step="1" value="${st.prewarm_pool||0}"></div>
          <div class="lc-num lc-bo"><label><input id="${P}-keepalive" type="checkbox" ${eff.startup&&eff.startup.keep_alive?"checked":""}><span>常驻锁（即便允许回收也常驻）</span></label></div>
        </div>
        ${grp("心跳保活（秒）")}
        <div class="lc-grid">
          <div class="lc-num"><span>心跳间隔</span><input id="${P}-hbint" type="number" min="1" step="1" value="${(hb.interval_ms||0)/1000}"></div>
          <div class="lc-num"><span>超时判定</span><input id="${P}-hbtout" type="number" min="1" step="1" value="${(hb.timeout_ms||0)/1000}"></div>
        </div>
        ${grp("空闲回收")}
        <div class="lc-grid">
          <div class="lc-num"><span>空闲阈值（分钟，0=不回收）</span><input id="${P}-idle" type="number" min="0" step="1" value="${(rc.max_idle_ms||0)/60000}"></div>
          <div class="lc-num"><span>优雅关闭时限（秒）</span><input id="${P}-grace" type="number" min="0" step="1" value="${(rc.graceful_shutdown_ms||0)/1000}"></div>
          <div class="lc-num lc-bo"><label><input id="${P}-recycle" type="checkbox" ${rc.idle_recycle?"checked":""}><span>允许空闲回收</span></label></div>
          <div class="lc-num lc-bo"><label><input id="${P}-bgtask" type="checkbox" ${rc.background_tasks?"checked":""}><span>有后台任务（跳过回收）</span></label></div>
        </div>
        ${grp("崩溃自愈 / 退避")}
        <div class="lc-grid">
          <div class="lc-num"><span>窗口内最大重启次数</span><input id="${P}-maxrst" type="number" min="0" step="1" value="${res.max_restarts||0}"></div>
          <div class="lc-num"><span>计数窗口（秒）</span><input id="${P}-winms" type="number" min="1" step="1" value="${(res.max_restarts_window_ms||0)/1000}"></div>
          <div class="lc-num"><span>退避基数（秒）</span><input id="${P}-bkbase" type="number" min="0" step="1" value="${(res.backoff_base_ms||0)/1000}"></div>
          <div class="lc-num"><span>退避上限（秒）</span><input id="${P}-bkmax" type="number" min="0" step="1" value="${(res.backoff_max_ms||0)/1000}"></div>
          <div class="lc-num"><span>正常退出码（逗号分隔）</span><input id="${P}-crashcodes" type="text" placeholder="0,1" value="${(res.crash_exit_codes||[]).join(",")}"></div>
          <div class="lc-num lc-bo"><label><input id="${P}-restart" type="checkbox" ${res.auto_restart?"checked":""}><span>崩溃自动重启</span></label></div>
        </div>
        ${grp("资源限制")}
        <div class="lc-grid">
          <div class="lc-num"><span>内存上限（MB，0=不限）</span><input id="${P}-mem" type="number" min="0" step="64" value="${Math.round((lim.mem_bytes||0)/1048576)}"></div>
          <div class="lc-num"><span>CPU 上限（%，0=不限）</span><input id="${P}-cpu" type="number" min="0" max="100" step="1" value="${lim.cpu_percent||0}"></div>
          <div class="lc-num"><span>单请求超时（秒，0=默认）</span><input id="${P}-req" type="number" min="0" step="1" value="${(lim.request_timeout_ms||0)/1000}"></div>
          <div class="lc-num"><span>stdout 单行上限（KB，默认 8192）</span><input id="${P}-line" type="number" min="1" step="64" value="${(lim.stdout_line_bytes||0)/1024}"></div>
        </div>
        <div class="row">
          <button data-save="1">保存并重启</button>
          <button class="outline" data-reset="1">恢复默认</button>
          <span data-lcmsg style="font-size:12px;color:var(--ink-soft)"></span>
        </div>`;
      // 事件委托：无论 box 何时被重渲染，点击都能命中（避免复用/竞态导致 onclick 未绑定或丢失）
      if (!box.__lcDeleg) {
        box.__lcDeleg = true;
        box.addEventListener("click", (e) => {
          const t = e.target;
          const id = box.__lcId;
          if (!id) return;
          if (t && t.dataset && t.dataset.save) doSave();
          else if (t && t.dataset && t.dataset.reset) doReset();
        });
      }
      box.__lcId = id;
      function msg(m) { const el = box.querySelector("[data-lcmsg]"); if (el) el.textContent = m; }
      async function doReset() {
        msg("恢复中…");
        try {
          await rpc("plugin.updateSettings", { pluginId: id, override: {} });
          msg("已恢复 manifest 默认并重启。");
          loadPerPluginList(); if (typeof refreshMgmt === "function") refreshMgmt();
        } catch (e) { msg("失败：" + e.message); }
      }
      async function doSave() {
        msg("保存中…");
        try {
          const g = (q) => box.querySelector("#lc-" + (box.id || "x") + "-" + id + "-" + q);
          const num = (q, unit) => { const v = g(q); if (!v) return null; const n = parseInt(v.value || "0", 10); if (isNaN(n) || n === 0) return null; return unit ? n * unit : n; };
          const codes = (g("crashcodes").value || "").split(",").map(s=>parseInt(s.trim(),10)).filter(n=>!isNaN(n));
          const override = {
            load_mode: g("mode").value,
            prewarm_pool: num("prewarm", 1),
            keep_alive: g("keepalive").checked,
            heartbeat_interval_ms: num("hbint", 1000),
            heartbeat_timeout_ms: num("hbtout", 1000),
            idle_recycle: g("recycle").checked,
            max_idle_ms: num("idle", 60000),
            graceful_shutdown_ms: num("grace", 1000),
            background_tasks: g("bgtask").checked,
            max_restarts: num("maxrst", 1),
            max_restarts_window_ms: num("winms", 1000),
            backoff_base_ms: num("bkbase", 1000),
            backoff_max_ms: num("bkmax", 1000),
            crash_exit_codes: codes.length ? codes : null,
            auto_restart: g("restart").checked,
            mem_bytes: num("mem", 1048576),
            cpu_percent: num("cpu", 1),
            request_timeout_ms: num("req", 1000),
            stdout_line_bytes: g("line") ? num("line", 1024) : null,
          };
          const r = await rpc("plugin.updateSettings", { pluginId: id, override });
          msg(`已保存并${g("mode").value === "disabled" ? "停止" : "重启"} · 状态 ${r.state}`);
          if (typeof refreshMgmt === "function") refreshMgmt();
        } catch (err) { console.error("save", id, err); msg("失败：" + (err && err.message || err)); }
      }
    }
    // 设置面板·单插件策略列表：每个插件一行汇总（模式/回收/重启/状态），点「编辑」展开编辑器
    const LC_INDEX: any = {}; // 插件显示名 -> id，供输入框解析
    async function loadPerPluginList(preserveSel = undefined) {
      const input = $("lcPick"), dl = $("lcPlugins");
      if (!input) return;
      try {
        const list = (await rpc("plugin.list", {})).plugins || [];
        LC_INDEX.__all = list;
        dl.innerHTML = list.map(p => `<option value="${p.name || p.pluginId}（${p.pluginId}）"></option>`).join("");
        // 保留之前选中
        if (preserveSel && input._sel) renderSinglePlugin(input._sel);
      } catch (e) { dl.innerHTML = ""; }
    }
    // 输入框解析：支持「名称（id）」「id」「部分名称/id 前缀»，返回命中插件或 null
    function resolveLC(raw) {
      const v = (raw || "").trim();
      if (!v) return null;
      const all = LC_INDEX.__all || [];
      // 1) 精确：独立 id
      let hit = all.find(p => p.pluginId === v);
      if (hit) return hit;
      // 2) 「名称（id）」/「名称(id)」：剥离括号取 id
      const m = v.match(/[（(]([^）)]+)[）)]\s*$/);
      if (m) { hit = all.find(p => p.pluginId === m[1]); if (hit) return hit; }
      // 3) 部分匹配：名称或 id 开头/包含（数字前缀优先 id）
      const lv = v.toLowerCase();
      hit = all.find(p => p.pluginId.toLowerCase().startsWith(lv)) ||
            all.find(p => (p.name || "").toLowerCase().startsWith(lv)) ||
            all.find(p => (p.pluginId + " " + (p.name || "")).toLowerCase().includes(lv));
      return hit || null;
    }
    // 针对某个插件：先显示一行当前配置摘要，再挂全部进程配置编辑器
    async function renderSinglePlugin(id) {
      const box = $("lcPerPluginBox");
      const input = $("lcPick"); if (input) input._sel = id;
      box.innerHTML = "读取中…";
      let v;
      try { v = await rpc("plugin.getLifecycle", { pluginId: id }); }
      catch (e) { box.innerHTML = `<span class="risk">读取失败：${e.message}</span>`; return; }
      const eff = v.effective || {}; const rc = eff.recycle || {}; const st = eff.startup || {};
      const hb = eff.heartbeat || {}; const lim = eff.limits || {}; const res = eff.resilience || {};
      const mode = {always:"常驻",prewarm:"预热",lazy:"按需",disabled:"禁用"}[eff.load_mode] || eff.load_mode;
      const over = v.override || {};
      const hasOver = Object.keys(over).some(k => over[k] !== null && over[k] !== undefined && over[k] !== "");
      const summary = `<div class="box" style="margin:10px 0;">当前：${mode} · 空闲${rc.idle_recycle ? "回收" : "不回收"} · ${res.auto_restart ? "自动重启" : "不自重启"} · 状态 ${v.state}${hasOver ? " · <span style='color:var(--cinnabar)'>已覆盖</span>" : ""}</div>`;
      box.innerHTML = summary + `<div id="lceeds-sel"></div>`;
      await toggleLifecycleEditor(id, $("lceeds-sel"), true); // 选中即强制渲染表单
    }
    // 组合框交互：输入联想（blur 解析、回车解析、datalist 由浏览器原生提供）
    function wirePluginPicker() {
      const input = $("lcPick");
      // 仅当解析出的插件与当前已渲染的不同时才重建表单。
      // 避免点“保存/恢复”按钮时先触发 blur → 重建整个 box 替换按钮 → 吞掉该次点击。
      const apply = () => {
        const hit = resolveLC(input.value);
        if (hit && hit.pluginId !== input._sel) renderSinglePlugin(hit.pluginId);
      };
      input.addEventListener("change", apply);
      input.addEventListener("blur", apply);
      input.addEventListener("keydown", (e) => { if (e.key === "Enter") apply(); });
    }
    // 全局预设：把 load_mode 批量应用到所有插件（保留用户对某个插件已设的其他覆盖）
    async function applyGlobalPreset(mode, opts) {
      $("lcGlobalResult").textContent = "应用中…";
      try {
        const list = (await rpc("plugin.list", {})).plugins || [];
        const idleMin = Math.max(parseInt($("gIdleMin").value || "5", 10), 1);
        const memM = parseInt($("gMemM").value || "0", 10);
        let done = 0;
        for (const p of list) {
          const isCore = PLUGIN_ROLE[p.pluginId] === "core";
          let loadMode = mode;
          if (mode === "core-others" && !isCore) loadMode = "lazy"; // presetCore → 其余按需
          const override = {
            load_mode: loadMode,
            idle_recycle: opts.recycle !== undefined ? opts.recycle : !!$("gRecycle").checked,
            max_idle_ms: idleMin * 60000,
            mem_bytes: memM ? memM * 1048576 : 0,
            auto_restart: opts.restart !== undefined ? opts.restart : !!$("gRestart").checked,
          };
          try { await rpc("plugin.updateSettings", { pluginId: p.pluginId, override }); done++; }
          catch (e) { /* 跳过单个失败 */ }
        }
        $("lcGlobalResult").textContent = `已应用到 ${done}/${list.length} 个插件。`;
      } catch (e) { $("lcGlobalResult").textContent = "失败：" + e.message; }
    }
    $("presetAlways").onclick = () => applyGlobalPreset("always", {});
    $("presetCore").onclick = () => applyGlobalPreset("core-others", {});
    $("presetLazy").onclick = () => applyGlobalPreset("lazy", {});
    $("btnLcRefresh").onclick = () => loadPerPluginList(true);
    $("btnMgmtRefresh").onclick = refreshMgmt;
    $("btnRelaunch").onclick = async () => {
      if (!confirm("确定重启应用？未保存的编辑内容可能丢失。")) return;
      await ipcRenderer.invoke("app:relaunch");
    };
    $("btnPick").onclick = async () => {
      try {
        const dir = await ipcRenderer.invoke("dialog:pickDir");
        if (dir.canceled) return;
        $("mgmtLog").textContent = "导入中：" + dir.path;
        const imp = await rpc("plugin.import", { srcPath: dir.path });
        $("mgmtLog").textContent = "已导入 → 检查依赖（若提示未就绪请点“依赖→安装并重启”）。";
        refreshMgmt();
        // 导入后立即重建侧栏与面板（否则要到 kernel:ready / 重启才出现）
        try { renderPlugins((await rpc("plugin.list", {})).plugins || []); } catch (e) {}
      } catch (e) { $("mgmtLog").textContent = "导入失败：" + e.message; }
    };
    // 「工具」Tab 切换与导入（kind:"tool" 进程工具，复用 dialog:pickDir + plugin.import）
    $("tabPlugins").onclick = () => switchAddTab("plugins");
    $("tabTools").onclick = () => switchAddTab("tools");
    $("btnToolRefresh").onclick = refreshTools;
    $("btnToolPick").onclick = async () => {
      try {
        const dir = await ipcRenderer.invoke("dialog:pickDir");
        if (dir.canceled) return;
        $("toolMgmtLog").textContent = "导入工具中：" + dir.path;
        await rpc("plugin.import", { srcPath: dir.path });
        $("toolMgmtLog").textContent = "已导入工具 → 若其 manifest.kind 为 \"tool\" 会显示在本面板。";
        refreshTools();
      } catch (e) { $("toolMgmtLog").textContent = "导入失败：" + e.message; }
    };

    function ensurePluginFrame(pid) {
      if (!pid || pid.startsWith("settings") || pid === "debug" || pid === "add") return;
      const frame: any = document.querySelector(`iframe[data-plugin="${pid}"]`);
      if (frame && !frame.getAttribute("src")) {
        const u = frame.dataset.src;
        if (u) frame.src = u; // 首次切到该插件时才加载，避免启动时把所有 lazy 插件全拉起
      }
    }
    // 左侧标签切换（统一入口，仅切换 .active 显示即可）
    function showPanel(name) {
      $$(".side-btn").forEach(b => b.classList.toggle("active", b.dataset.panel === name));
      $$(".panel").forEach(p => p.classList.toggle("active", p.id === "panel-" + name));
      if (name.startsWith("plugin.")) ensurePluginFrame(name.slice("plugin.".length));
    }
    $$(".side-btn").forEach(btn => btn.onclick = () => showPanel(btn.dataset.panel));

    const WEEK = ["日","一","二","三","四","五","六"];
    function tick() {
      const d = new Date();
      $("clock").textContent = [d.getHours(),d.getMinutes()].map(x=>String(x).padStart(2,"0")).join(":");
      $("week").textContent = d.toLocaleDateString("zh-CN",{month:"long"}) + " · 星期" + WEEK[d.getDay()];
    }
    tick(); setInterval(tick, 1000);

    ipcRenderer.on("kernel:ready", async (e, { list, resBase, kernelBase, port, token }) => {
      setStatus("内核已连接", "ok");
      if (resBase) $("logo").src = resBase + "logo/logo128.png"; // 阶段E：资源服务
      KERNEL_BASE = kernelBase; OCT_CONN = { port, token };
      $("list").textContent = JSON.stringify(list, null, 2);
      await loadUIPrefs(); // §17.2 B：先读取 user-settings.json 的侧栏顺序/默认页再渲染
      renderPlugins(list && list.plugins || []);
      loadDepCfg();
      refreshFns();
      refreshMgmt();
      refreshTools();
      wirePluginPicker(); // 搜索+下拉选择交互（只挂一次，loadPerPluginList 只重建选项）
      loadPerPluginList();
      openDefaultPlugin(); // 启动后默认打开用户在设置中指定的插件页（默认 home）
      refreshOrderSettings(); // 填充「插件排序 · 默认页」配置卡片
      loadHotkeyCache();      // 阶段L：缓存热键表（app 作用域热键由渲染进程派发）
    });

    async function loadDepCfg() {
      try {
        const c = await rpc("runtime.getConfig", {});
        $("cfgIndex").value = (c.index_urls || []).join("\n");
        $("cfgCache").value = c.cache_dir || "";
      } catch (e) { $("cfgResult").textContent = "读取设置失败：" + e.message; }
    }
    $("btnSaveCfg").onclick = async () => {
      const indexUrls = $("cfgIndex").value.split("\n").map(s => s.trim()).filter(Boolean);
      try {
        await rpc("runtime.setConfig", { index_urls: indexUrls, cache_dir: $("cfgCache").value.trim() });
        $("cfgResult").textContent = "已保存，下次安装依赖生效。" + (indexUrls.length ? `（${indexUrls[0]}）` : "（用官方源）");
      } catch (e) { $("cfgResult").textContent = "保存失败：" + e.message; }
    };

    // ── 插件排序 · 默认页 设置 ──
    function defaultOrder(plugins) {
      const ids = plugins.map(p => p.pluginId);
      return ["home", ...ids.filter(i => i !== "home")];
    }
    let orderSelIndex = -1;
    // 名称映射缓存：避免重绘/排序时因拿不到 plugins 列表而退化成英文 id。
    let ORDER_NAMES = {};
    function rebuildOrderList(order, plugins = undefined) {
      if (plugins) { ORDER_NAMES = {}; plugins.forEach(p => ORDER_NAMES[p.pluginId] = p.name || p.pluginId); }
      const ul = $("orderList"); ul.innerHTML = "";
      (order || []).forEach((id, i) => {
        const li = document.createElement("li");
        li.dataset.id = id;
        li.style.cssText = "display:flex;align-items:center;gap:10px;padding:6px 8px;border-radius:6px;"
          + "user-select:none;-webkit-user-select:none;cursor:pointer;";
        const h = document.createElement("span");
        h.className = "order-hand";
        h.style.cssText = "display:inline-flex;align-items:center;cursor:grab;flex:0 0 18px;opacity:.72;touch-action:none;";
        h.innerHTML = `<img src="../resources/icons/menu-order.svg" alt="拖拽排序" style="width:16px;height:16px;pointer-events:none;">`;
        const t = document.createElement("span");
        t.style.cssText = "flex:1;";
        t.textContent = `${i + 1}. ${ORDER_NAMES[id] || id}`;
        li.appendChild(h); li.appendChild(t);
        li.onclick = () => { ul.querySelectorAll("li").forEach(x => x.classList.remove("active")); li.classList.add("active"); orderSelIndex = i; };
        ul.appendChild(li);
      });
      orderSelIndex = -1;
    }
    // ── 通用可拖拽列表 DragList(ul, opts) ──
    // 让任意 <ul>（其 <li> 内含把手选择器可选）支持 Pointer 拖拽重排，供所有需"拖拽排序"的列表统一复用。
    // opts: { handle:'.order-hand', ghost:true, onChange:function }。
    //   handle   —— 拖拽把手选择器（仅按住它才进入拖拽）
    //   ghost    —— 拖动时是否显示跟随鼠标的虚影（样式类 .order-ghost）
    //   onChange —— 每次重排后回调（可用来实时刷新预览/数据），不含首次按下
    function DragList(ul, opts) {
      if (!ul) return null;
      const o = Object.assign({ handle: ".order-hand", ghost: true, onChange: null }, opts || {});
      let drag = null, ghost = null;
      ul.addEventListener("pointerdown", e => {
        const h = e.target.closest ? e.target.closest(o.handle) : null;
        if (!h || !h.closest("li")) return;
        const li = h.closest("li");
        li.classList.add("dragging");
        try { h.setPointerCapture(e.pointerId); } catch (_) {}
        drag = li;
        if (o.ghost) {
          ghost = document.createElement("div");
          ghost.className = "order-ghost";
          ghost.textContent = li.textContent.trim();
          document.body.appendChild(ghost);
        }
      });
      ul.addEventListener("pointermove", e => {
        if (!drag) return;
        if (ghost) { ghost.style.left = e.clientX + "px"; ghost.style.top = e.clientY + "px"; }
        const hit = document.elementFromPoint(e.clientX, e.clientY);
        const liRed = hit && hit.closest ? hit.closest("li") : null;
        if (!liRed || !ul.contains(liRed) || liRed === drag) return;
        const r = liRed.getBoundingClientRect();
        ul.insertBefore(drag, e.clientY < r.top + r.height / 2 ? liRed : liRed.nextSibling);
        if (o.onChange) o.onChange();
      });
      const end = () => {
        if (drag) drag.classList.remove("dragging");
        drag = null;
        if (ghost) { ghost.remove(); ghost = null; }
      };
      ul.addEventListener("pointerup", end);
      ul.addEventListener("pointercancel", end);
      return ul;
    }
    // 排序列表复用 DragList：拖动时实时刷新「侧栏当前顺序」预览。
    DragList($("orderList"), { onChange: () => {
      const order = [...$("orderList").querySelectorAll("li")].map(l => l.dataset.id);
      refreshOrderResult(order);
    } });
    async function refreshOrderSettings() {
      await loadUIPrefs();
      let plugins = []; try { plugins = (await rpc("plugin.list", {})).plugins || []; } catch {}
      const order = (uiPrefs.sidebarOrder && uiPrefs.sidebarOrder.length) ? uiPrefs.sidebarOrder : defaultOrder(plugins);
      const sel = $("defPageSel"); sel.innerHTML = "";
      plugins.forEach(p => {
        const o = document.createElement("option"); o.value = p.pluginId;
        o.textContent = (p.name || p.pluginId) + " (" + p.pluginId + ")"; sel.appendChild(o);
      });
      if (sel.querySelector('option[value="' + defaultPageId() + '"]')) sel.value = defaultPageId();
      else sel.value = "home";
      rebuildOrderList(order, plugins);
      refreshOrderResult(order);
    }
    function applySidebarOrder() {
      const sep = document.querySelector("nav.side .nav-title.plugin-nav");
      if (!sep) return;
      const sideroot = document.querySelector("nav.side");
      const btns = [...$$("button.side-btn.plugin-nav")];
      const order = uiPrefs.sidebarOrder || [];
      const rank = {}; order.forEach((id, i) => rank[id] = i); let next = order.length;
      const r = b => (b.dataset.pluginId in rank) ? rank[b.dataset.pluginId] : next++;
      btns.sort((a, b) => r(a) - r(b));
      let anchor = sep;
      btns.forEach(b => { sideroot.insertBefore(b, anchor.nextSibling); anchor = b; });
    }
    function moveOrder(delta) {
      const li = $("orderList").querySelector("li.active");
      if (!li) return;
      const tgt = delta < 0 ? li.previousElementSibling : li.nextElementSibling;
      if (!tgt) return;
      $("orderList").insertBefore(delta < 0 ? li : tgt, delta < 0 ? tgt : li);
      const order = [...$("orderList").querySelectorAll("li")].map(l => l.dataset.id);
      rebuildOrderList(order); refreshOrderResult(order);
    }
    $("btnOrderUp").onclick = () => moveOrder(-1);
    $("btnOrderDown").onclick = () => moveOrder(1);
    function refreshOrderResult(order) { $("orderResult").textContent = "侧栏当前顺序：" + ((order||[]).map(id => ORDER_NAMES[id] || id).join(" → ") || "空"); }
    $("btnOrderReset").onclick = async () => {
      await saveUIPrefs({ sidebarOrder: [], defaultPage: "home" });
      refreshOrderSettings(); applySidebarOrder(); $("orderResult").textContent = "已恢复默认：home 置首，其余按原序。";
    };
    $("btnOrderSave").onclick = async () => {
      const order = [...$("orderList").querySelectorAll("li")].map(l => l.dataset.id);
      await saveUIPrefs({ defaultPage: $("defPageSel").value || "home", sidebarOrder: order });
      applySidebarOrder(); $("orderResult").textContent = "已保存并应用。下次启动默认打开：" + ($("defPageSel").value || "home");
    };

    function depPluginsReady() {
      const input = $("depPick"), dl = $("depPlugins");
      if (!input) return false;
      if (!$("depPlugins").innerHTML) {
        const all = LC_INDEX.__all || [];
        dl.innerHTML = all.map(p => `<option value="${p.name || p.pluginId}（${p.pluginId}）"></option>`).join("");
      }
      return true;
    }

    // ── §17.2 A · 插件内部设置页（settingsSchema 驱动，非 schema 字段不得写入） ──
    // 表单由 manifest.settingsSchema.properties 派生：string/number/integer/boolean/enum → 对应控件。
    // 值默认取 effective.settings（user → defaults → 内置），保存时仅提交 schema 声明的键。
    async function loadPluginSettingsList() {
      const input = $("psPick"), dl = $("psPlugins");
      if (!input) return;
      try {
        const list = (await rpc("plugin.list", {})).plugins || [];
        LC_INDEX.__all = list;
        dl.innerHTML = list.map(p => `<option value="${p.name || p.pluginId}（${p.pluginId}）"></option>`).join("");
      } catch (e) { dl.innerHTML = ""; }
    }
    function schemaPropControl(key: string, prop: any, current: any) {
      const label = (prop && (prop.title || prop.description) || key);
      const id = "ps-f-" + key;
      const wrap = document.createElement("div");
      wrap.className = "row"; wrap.style.cssText = "flex-wrap:wrap;gap:8px;align-items:center;";
      const lab = document.createElement("label"); lab.setAttribute("for", id); lab.textContent = label + "："; lab.style.cssText = "min-width:140px;color:var(--ink);";
      wrap.appendChild(lab);
      const typ = (prop && prop.type) || "string";
      let ctl: HTMLElement;
      if (prop && Array.isArray(prop.enum)) {
        ctl = document.createElement("select"); ctl.id = id;
        prop.enum.forEach((ev: any) => {
          const o = document.createElement("option"); o.value = ev; o.textContent = String(ev);
          if (String(ev) === String(current)) o.selected = true;
          ctl.appendChild(o);
        });
      } else if (typ === "boolean") {
        const cb = document.createElement("input"); cb.type = "checkbox"; cb.id = id;
        cb.checked = !!current;
        ctl = cb;
      } else {
        const it = document.createElement("input");
        it.id = id;
        it.type = (typ === "number" || typ === "integer") ? "number" : "text";
        if (prop && prop.pattern) it.pattern = prop.pattern;
        it.value = (current === undefined || current === null) ? "" : String(current);
        ctl = it;
      }
      ctl.style.cssText = "flex:1;min-width:200px;";
      wrap.appendChild(ctl);
      if (prop && prop.description && prop.description !== label) {
        const tip = document.createElement("span"); tip.className = "hint"; tip.textContent = prop.description;
        tip.style.cssText = "width:100%;font-size:12px;color:var(--ink-soft);";
        wrap.appendChild(tip);
      }
      return wrap;
    }
    async function renderPluginSettings(pid) {
      const box = $("psArea"); if (!box) return;
      box.innerHTML = "读取中…";
      let v;
      try { v = await rpc("plugin.getSettings", { pluginId: pid }); }
      catch (e) { box.innerHTML = `<span class="risk">读取失败：${e.message}</span>`; return; }
      const schema = v.schema || {};
      let props: Record<string, any> = {};
      if (typeof schema === "object" && schema !== null && schema.properties) props = schema.properties;
      const keys = Object.keys(props);
      if (!keys.length) {
        box.innerHTML = `<span class="hint">插件 <b>${pid}</b> 未在 manifest 中声明 settingsSchema，无内部设置可编辑。</span>` +
          (v.effective && v.effective.settings && Object.keys(v.effective.settings).length
            ? `<div class="box" style="margin-top:8px"><b>当前生效设置：</b><pre id="psRaw" style="font-size:12px">${JSON.stringify(v.effective.settings, null, 2)}</pre></div>` : "");
        return;
      }
      const eff = (v.effective && v.effective.settings) || {};
      box.innerHTML = "";
      const form = document.createElement("div"); form.className = "box";
      for (const k of keys) {
        form.appendChild(schemaPropControl(k, props[k], eff[k]));
      }
      box.appendChild(form);
      const btns = document.createElement("div"); btns.className = "row"; btns.style.cssText = "margin-top:10px;";
      const save = document.createElement("button"); save.textContent = "保存设置";
      save.onclick = async () => {
        const settings: Record<string, any> = {};
        for (const k of keys) {
          const el = document.getElementById("ps-f-" + k) as any;
          if (!el) continue;
          const prop = props[k];
          if (prop && Array.isArray(prop.enum)) {
            if (el.value !== "") settings[k] = el.selectedOptions[0].value;
          } else if (prop && prop.type === "boolean") {
            settings[k] = el.checked;
          } else {
            const raw = (el.value || "").trim();
            if (prop && (prop.type === "number" || prop.type === "integer")) {
              settings[k] = raw === "" ? "" : (prop.type === "integer" ? parseInt(raw, 10) : parseFloat(raw));
            } else {
              settings[k] = raw;
            }
          }
        }
        try {
          await rpc("plugin.setSettings", { pluginId: pid, settings });
          $("psResult").textContent = "已保存。schema 未覆盖的字段已被内核过滤。" + (eff.auto_restart ? "" : "");
        } catch (e) { $("psResult").textContent = "保存失败：" + e.message; }
      };
      btns.appendChild(save);
      const rst = document.createElement("button"); rst.className = "outline"; rst.textContent = "恢复默认";
      rst.onclick = async () => {
        try { await rpc("plugin.setSettings", { pluginId: pid, settings: {} }); } catch (e) {}
        $("psResult").textContent = "已清空用户覆盖，恢复 manifest 默认。"; renderPluginSettings(pid);
      };
      btns.appendChild(rst);
      box.appendChild(btns);
      const res = document.createElement("pre"); res.id = "psResult"; res.className = "hint"; res.style.cssText = "font-size:12px;margin-top:8px;"; res.textContent = "当前生效值已预填，保存后仅写入 schema 覆盖字段。";
      box.appendChild(res);
    }
    function wirePluginSettingsPicker() {
      const input = $("psPick"); if (!input) return;
      const apply = () => {
        const hit = resolveLC(input.value);
        if (hit) renderPluginSettings(hit.pluginId);
      };
      input.addEventListener("change", apply);
      input.addEventListener("blur", apply);
      input.addEventListener("keydown", (e) => { if (e.key === "Enter") apply(); });
    }
    if ($("psPick")) { loadPluginSettingsList(); wirePluginSettingsPicker(); }
    $("btnDepRefresh").onclick = async () => {
      $("depPlugins").innerHTML = "";
      try { const l = (await rpc("plugin.list", {})).plugins || []; LC_INDEX.__all = l; } catch (e) {}
      depPluginsReady();
    };
    // 当前选中的依赖插件：支持「名称（id）」「id」「部分名称/id 前缀」
    function depPluginId() {
      const v = $("depPick").value.trim();
      const hit = resolveLC(v);
      return (hit && hit.pluginId) || v;
    }
    $("btnDeps").onclick = async () => {
      depPluginsReady();
      const pid = depPluginId();
      if (!pid) { $("depResult").textContent = "请先在“插件依赖”处选择或输入一个插件。"; return; }
      try {
        const r = await rpc("runtime.preview", { pluginId: pid });
        $("depArea").textContent = r.satisfied
          ? (pid + " · 依赖已就绪（隔离环境存在）")
          : ("待安装依赖：\n" + JSON.stringify(r.dependencies, null, 2));
        $("depResult").textContent = pid + " · 就绪：" + r.satisfied;
      } catch (e) { $("depResult").textContent = "预览失败：" + e.message; }
    };

    $("btnInstall").onclick = async () => {
      depPluginsReady();
      const pid = depPluginId();
      if (!pid) { $("depResult").textContent = "请先在“插件依赖”处选择或输入一个插件。"; return; }
      $("depResult").textContent = "安装中…（" + pid + " · 首次/重装需数分钟，请耐心等待）";
      try {
        const r = await rpc("runtime.install", { pluginId: pid, force: true }, 600000);
        $("depResult").textContent = "已安装 → " + r.venvPython;
        await rpc("plugin.restart", { pluginId: pid });
        $("depResult").textContent += "\n插件已重启，切换为隔离解释器。";
      } catch (e) { $("depResult").textContent = "安装失败：" + e.message; }
    };

    $("btnEnvsClear").onclick = () => $("envsResult").textContent = "已清空。";

    $("btnEnvs").onclick = async () => {
      const pre = $("envsResult");
      pre.textContent = "读取中…（需内核已重建并重启）";
      try {
        const r = await rpc("runtime.envs", {});
        pre.textContent = renderEnvs(r);
      } catch (e) { pre.textContent = "读取失败：" + e.message; }
    };

    function renderEnvs(r) {
      const lines = [];
      const section = t => lines.push("──── " + t + " ────");
      const pkg = p => lines.push("  " + p.name + "   " + p.version);
      if (r.managed) {
        section("项目 Python（托管 3.12）");
        if (r.managed.error) lines.push("  错误: " + r.managed.error);
        (r.managed.packages || []).forEach(pkg);
      }
      (r.plugins || []).forEach(pl => {
        section("插件 " + pl.name + "（" + pl.pluginId + "）");
        if (pl.error) lines.push("  错误: " + pl.error);
        (pl.packages || []).forEach(pkg);
      });
      if (r.node) {
        section("Electron 宿主（host）");
        if (r.node.error) lines.push("  错误: " + r.node.error);
        (r.node.packages || []).forEach(pkg);
      }
      return lines.join("\n") || "（无）";
    }

    // 共享函数卡（阶段C）
    async function refreshFns() {
      try {
        const r = await rpc("registry.list", {});
        const fns = r.functions || [];
        if (!fns.length) { $("fnArea").textContent = "（暂无已注册共享函数）"; return; }
        $("fnArea").innerHTML = fns.map(f =>
          `<div class="perm"><input type="checkbox" data-name="${f.name}">` +
          `<span class="mono">${f.name}</span><span class="risk" style="color:var(--ink-faint);border-color:var(--line);background:transparent">${f.pluginId}</span>` +
          `<span style="color:var(--ink-faint);font-size:11px">${f.desc}</span></div>`).join("");
      } catch (e) { $("fnArea").textContent = "读取失败：" + e.message; }
    }
    $("btnRegRefresh").onclick = refreshFns;
    $("btnCallFn").onclick = async () => {
      const chosen = $("fnArea").querySelector("input[type=checkbox]:checked");
      if (!chosen) { $("fnResult").textContent = "请先勾选一个共享函数。"; return; }
      const name = chosen.dataset.name;
      try {
        const r = await rpc("registry.call", { name, params: {} });
        $("fnResult").textContent = `调用 ${name} → ` + JSON.stringify(r.result, null, 2);
      } catch (e) { $("fnResult").textContent = `调用 ${name} 失败：` + e.message; }
    };

    // ── 阶段K · LibreOffice 引擎（作为「外部地址与内存设置」中的一种资源，随选择进入编辑器）──
    async function loStatus() {
      try {
        const r = await rpc("plugin.call", { pluginId: "conversion", method: "conversion.libreoffice_status", params: {} });
        return r.result || {};
      } catch (e) { return { error: e.message }; }
    }
    async function loSave(cfg) {
      try {
        const r = await rpc("plugin.call", { pluginId: "conversion", method: "conversion.libreoffice_set", params: cfg });
        return r.result || {};
      } catch (e) { return { error: e.message }; }
    }
    // 在资源编辑器内自包含渲染 LibreOffice 引擎面板并绑定事件（不依赖全局 id）
    function renderLoPanel(box) {
      const g = (sel) => box.querySelector(sel);
      const radioRow = g(".lo-src-row"), dirRow = g(".lo-dir-row"), msiRow = g(".lo-msi-row");
      const dirIn = g("[data-lo-dir]"), msiIn = g("[data-lo-msi]"), urlIn = g("[data-lo-url]");
      const statusEl = g("[data-lo-status]"), barEl = g("[data-lo-bar]");
      const toggleRows = (src) => { dirRow.hidden = src !== "dir"; msiRow.hidden = src !== "msi"; };
      const renderStatus = (S) => {
        const st = S.state || {}, bin = S.bin || "";
        const lines = [];
        if (bin) lines.push("✅ 已可用: " + bin); else lines.push("⚠ 未检测到可用 soffice");
        if (st.phase === "download") lines.push(`⬇ 下载中 ${st.percent}%（${st.loaded_mb} / ${st.total_mb} MB）`);
        else if (st.phase === "extract") lines.push("🔧 解压中…");
        else if (st.phase === "done") lines.push("✅ 安装完成");
        if (st.error) lines.push("❌ " + st.error);
        statusEl.textContent = lines.join("\n") || "—";
        barEl.style.width = (st.running ? (st.percent || 0) : 0) + "%";
      };
      let timer = null;
      const poll = async () => { const S = await loStatus(); renderStatus(S); if (!(S.state && S.state.running)) { clearInterval(timer); timer = null; } };
      const collect = () => ({
        source: (box.querySelector('input[name="loSource"]:checked') || {}).value || "auto",
        soffice_dir: dirIn.value.trim(), msi_path: msiIn.value.trim(), url: urlIn.value.trim(),
      });
      const load = async () => {
        const S = await loStatus();
        if (S.error) { statusEl.textContent = "读取失败：" + S.error; return; }
        const cfg = S.config || {}, source = cfg.source || "auto";
        box.querySelectorAll('input[name="loSource"]').forEach(r => { r.checked = (r.value === source); });
        dirIn.value = cfg.soffice_dir || ""; msiIn.value = cfg.msi_path || ""; urlIn.value = cfg.url || "";
        toggleRows(source); renderStatus(S);
      };
      radioRow.addEventListener("change", (e) => toggleRows(e.target.value));
      g("[data-lo-dirpick]").onclick = async () => { const r = await ipcRenderer.invoke("dialog:pickDir"); if (r && !r.canceled && r.path) dirIn.value = r.path; };
      g("[data-lo-msipick]").onclick = async () => { const r = await ipcRenderer.invoke("dialog:pickFile", [{ name: "MSI 安装包", extensions: ["msi"] }]); if (r && !r.canceled && r.path) msiIn.value = r.path; };
      g("[data-lo-clear]").onclick = async () => { await loSave({ source: "auto", soffice_dir: "", msi_path: "", url: "" }); load(); };
      g("[data-lo-apply]").onclick = async () => { const S = await loSave(collect()); renderStatus(S); statusEl.textContent = "✅ 配置已保存\n" + statusEl.textContent; };
      g("[data-lo-install]").onclick = async () => {
        await loSave(collect());
        const S = (await rpc("plugin.call", { pluginId: "conversion", method: "conversion.libreoffice_install", params: {} })).result || {};
        renderStatus(S);
        if (timer) clearInterval(timer);
        timer = setInterval(poll, 600);
      };
      load();
    }

    // ── 阶段K · 外部地址与内存设置（宿主直接扫描各插件静态 manifest，不依赖插件进程）──
    const LOAD_OPTS = [["lazy", "懒加载（默认）"], ["startup", "启动时"], ["tab", "进页面时"]];
    const UNLOAD_OPTS = [["idle", "闲置后自动释放"], ["once", "用完即退"], ["keep", "常驻不释放"]];
    const RS_INDEX: any = {}; // 资源 key -> item，供输入框解析
    async function resScan() {
      try { return (await ipcRenderer.invoke("res:scan")).groups || []; }
      catch (e) { return []; }
    }
    function resolveRes(raw) {
      const v = (raw || "").trim(); if (!v) return null;
      const all = RS_INDEX.__all || [];
      let hit = all.find(x => x.key === v); if (hit) return hit;
      const m = v.match(/[（(]([^）)]+)[）)]\s*$/);
      if (m) { hit = all.find(x => x.key === m[1]); if (hit) return hit; }
      const lv = v.toLowerCase();
      hit = all.find(x => x.key.toLowerCase().includes(lv)) ||
            all.find(x => (x.label || "").toLowerCase().includes(lv));
      return hit || null;
    }
    function resetResEditor() {
      const box = $("resEditBox");
      if (box) box.innerHTML = '<span style="font-size:12px;color:var(--ink-soft)">👆 从上方选择 AI 模型或外部二进制，再在此配置其安装地址与内存时机。</span>';
    }
    function renderResourceEditor(item) {
      const box = $("resEditBox");
      const grp = (t) => `<div class="lc-grp">${t}</div>`;
      // LibreOffice 引擎：作为特殊变体，编辑器内直接呈现其下载/安装/状态面板
      if (item.variant === "lo") {
        box.innerHTML = `
          <div class="box" style="margin-top:10px">
            <div style="font-size:13px;font-weight:600">${item.label}
              <span style="font-size:11px;color:var(--ink-soft);margin-left:6px">外部二进制 · ${item.pid}</span>
            </div>
            <div class="lc-grp">安装来源</div>
            <div class="row lo-src-row" style="flex-wrap:wrap;gap:8px">
              <label class="lo-src"><input type="radio" name="loSource" value="auto"> 自动下载官方版</label>
              <label class="lo-src"><input type="radio" name="loSource" value="dir"> 本地已安装目录</label>
              <label class="lo-src"><input type="radio" name="loSource" value="msi"> 本地 MSI 安装包</label>
            </div>
            <div class="box lo-dir-row" hidden>
              <div class="row"><input type="text" data-lo-dir placeholder="选择已安装的 LibreOffice 根目录（含 program/soffice.exe）" style="flex:1"><button class="mini" data-lo-dirpick>浏览目录</button></div>
            </div>
            <div class="box lo-msi-row" hidden>
              <div class="row"><input type="text" data-lo-msi placeholder="选择 LibreOffice MSI 安装包（.msi）" style="flex:1"><button class="mini" data-lo-msipick>浏览文件</button></div>
            </div>
            <div class="box">
              <div class="row"><input type="text" data-lo-url placeholder="官方镜像地址（可留空）" style="flex:1"></div>
              <div class="row">
                <button class="outline" data-lo-clear>清空配置</button>
                <button data-lo-apply>应用配置</button>
                <button data-lo-install>开始下载/安装</button>
              </div>
            </div>
            <div class="box" data-lo-status style="white-space:pre-wrap;font-size:12px">加载中…</div>
            <div class="lo-progress"><div class="lo-progress-bar" data-lo-bar style="width:0%"></div></div>
          </div>`;
        renderLoPanel(box);
        return;
      }
      const P = "res-" + item.pid + "-" + item.key;
      const isModel = item.type === "model";
      box.innerHTML = `
        <div class="box" style="margin-top:10px">
          <div style="font-size:13px;font-weight:600">${item.label}
            <span style="font-size:11px;color:var(--ink-soft);margin-left:6px">${isModel ? "AI 模型" : "外部二进制"} · ${item.pid}</span>
          </div>
          ${grp("安装地址")}
          <div class="lc-grid">
            <div class="lc-num" style="grid-column:1/-1"><span>路径（留空 = 自动探测）</span>
              <div style="display:flex;gap:8px"><input id="${P}-path" type="text" placeholder="${isModel ? "选择模型目录" : "选择可执行文件"}" style="flex:1">
                <button class="mini" data-r-browse>浏览</button></div>
            </div>
          </div>
          ${grp("加载时机")}
          <div class="lc-grid">
            <div class="lc-num"><span>何时加载</span><select id="${P}-load" class="lc-sel">${LOAD_OPTS.map(([v, t]) => `<option value="${v}"${v === item.load ? " selected" : ""}>${t}</option>`).join("")}</select></div>
          </div>
          ${grp("结束时机（闲置释放）")}
          <div class="lc-grid">
            <div class="lc-num"><span>何时结束</span><select id="${P}-unload" class="lc-sel">${UNLOAD_OPTS.map(([v, t]) => `<option value="${v}"${v === item.unload ? " selected" : ""}>${t}</option>`).join("")}</select></div>
            <div class="lc-num" data-idle-wrap><span>闲置多少分钟后释放</span><input id="${P}-idle" type="number" min="1" step="1" value="${item.idle_min || 10}"></div>
          </div>
          <div class="row" style="margin-top:12px">
            <button data-r-save>保存</button>
            <span data-r-msg style="font-size:12px;color:var(--ink-soft)"></span>
          </div>
        </div>`;
      $(P + "-path").value = item.path || "";
      const idleWrap = box.querySelector("[data-idle-wrap]");
      const toggleIdle = () => { idleWrap.style.display = $(P + "-unload").value === "idle" ? "" : "none"; };
      $(P + "-unload").addEventListener("change", toggleIdle);
      toggleIdle();
      box.querySelector("[data-r-browse]").onclick = async () => {
        const r = await ipcRenderer.invoke(isModel ? "dialog:pickDir" : "dialog:pickFile");
        if (r && !r.canceled && r.path) $(P + "-path").value = r.path;
      };
      box.querySelector("[data-r-save]").onclick = async () => {
        const cfg = {
          key: item.key,
          path: $(P + "-path").value.trim(),
          load: $(P + "-load").value,
          unload: $(P + "-unload").value,
          idle_min: $(P + "-idle").value,
        };
        const btn = box.querySelector("[data-r-save]"), msg = box.querySelector("[data-r-msg]");
        btn.textContent = "保存…";
        try {
          const r = await ipcRenderer.invoke("res:set", { pid: item.pid, item: cfg });
          if (r && r.ok) { msg.textContent = "已保存 ✓"; }
          else { msg.textContent = "失败：" + ((r && r.error) || ""); }
          btn.textContent = "保存";
        } catch (e) { msg.textContent = "失败：" + e.message; btn.textContent = "保存"; }
      };
    }
    function wireResPicker() {
      const input = $("resPick");
      if (!input || input._wired) return;
      input._wired = true;
      const apply = () => {
        const hit = resolveRes(input.value);
        if (hit && hit.key !== input._sel) renderResourceEditor(hit);
        input._sel = hit ? hit.key : null;
      };
      input.addEventListener("change", apply);
      input.addEventListener("blur", apply);
      input.addEventListener("keydown", (e) => { if (e.key === "Enter") apply(); });
      const refresh = $("btnResRefresh");
      if (refresh) refresh.onclick = () => loadResources(true);
    }
    async function loadResources(preserve = undefined) {
      const all = await resScan();
      RS_INDEX.__all = all;
      const dl = $("resOpts");
      if (dl) dl.innerHTML = all.map(x => `<option value="${x.label}（${x.key}）"></option>`).join("");
      const sel = preserve && $("resPick") ? $("resPick")._sel : null;
      const cur = sel ? all.find(x => x.key === sel) : null;
      if (cur) { renderResourceEditor(cur); }
      else { resetResEditor(); }
      wireResPicker();
    }

    ipcRenderer.on("kernel:error", (e, err) => { setStatus("启动失败", "err"); });
    // 内核转发的 notification（event）：仅处理插件进程状态，刷新左侧栏圆点
    ipcRenderer.on("kernel:event", (e, raw) => {
      let t;
      try { t = JSON.parse(raw); } catch { console.log("event:", raw); return; }
      if (t.method !== "event") { console.log("event:", raw); return; }
      const p = t.params || {};
      console.log(`[event] ${p.source}:${p.type}`, p.data);
      // 内核进程状态事件：仅刷新圆点（颜色只信内核 State，不做乐观猜测）。
      if (p.source === "kernel" && p.type === "plugin.state" && p.data && p.data.id) {
        const b = document.querySelector(`nav.side .side-btn[data-plugin-id="${CSS.escape ? CSS.escape(p.data.id) : p.data.id}"]`);
        if (b) paintDot(b, p.data.state, b.classList.contains("disabled"));
      }
      // 通用 tool 事件转发：把 tool(source) 发起的事件投给订阅它的插件 iframe。
      // 跳过 cursor 高频帧（放大镜画面由宿主直接投给 overlay 窗渲染），只把业务/收尾事件送插件页。
      if (p.source && p.source !== "kernel") {
        const pids = toolSubs.get(p.source);
        if (pids && pids.size) {
          let inner = p.data;
          if (typeof inner === "string") { try { inner = JSON.parse(inner); } catch (e) { inner = null; } }
          if (!(inner && inner.type === "cursor")) {
            pids.forEach((pid) => forwardToPlugin(pid, { type: "oct:tool:event", source: p.source, dataType: p.type, data: p.data }));
          }
        }
      }
    });

    // ── 阶段L · 热键（快捷键）：动态扫描插件 manifest.json 的 hotkeys 声明 ──
    // 声明 = 插件静态文件；用户改键/禁用 = state/hotkeys.json（宿主主进程读写并注册全局键）。
    let HK_ITEMS = [];       // 最终生效热键表（声明 + 用户覆盖）
    let HK_REPORT = [];      // 逐条注册结果：ok / disabled / conflict / invalid / app
    let HK_STORE = "state/hotkeys.json";
    let hkRec = null;        // 正在录制的行
    const hkEsc = (s) => String(s == null ? "" : s).replace(/[&<>"']/g,
      c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
    const HK_BADGE = { ok: "已注册", disabled: "已禁用", conflict: "冲突", invalid: "非法", app: "应用内生效" };

    // 浏览器按键事件 → 组合键字符串（主进程还会再规范化校验一次）
    function comboFromEvent(e) {
      const mods = [];
      if (e.ctrlKey) mods.push("Ctrl");
      if (e.altKey) mods.push("Alt");
      if (e.shiftKey) mods.push("Shift");
      if (e.metaKey) mods.push("Meta");
      const alias = { " ": "Space", "Escape": "Esc", "ArrowUp": "Up", "ArrowDown": "Down", "ArrowLeft": "Left", "ArrowRight": "Right" };
      let k = alias[e.key] || e.key || "";
      if (/^[a-z]$/i.test(k)) k = k.toUpperCase();
      // 只按了修饰键时返回空串，继续等待主键
      if (!k || ["Control", "Alt", "Shift", "Meta", "AltGraph", "CapsLock", "Dead", "Unidentified"].includes(k)) return "";
      return mods.concat([k]).join("+");
    }

    function hkReportText() {
      const ok = HK_REPORT.filter(r => r.status === "ok").length;
      const bad = HK_REPORT.filter(r => r.status === "conflict" || r.status === "invalid");
      let t = "已注册全局热键 " + ok + " / 共 " + HK_REPORT.length + " 条 · 存储：" + HK_STORE;
      if (bad.length) t += "\n未生效：" + bad.map(b => b.pluginId + "::" + b.id + " → " + (b.error || b.status)).join("；");
      return t;
    }

    async function refreshHotkeys() {
      const box = $("hkList");
      if (!box) return;
      try {
        const r = await ipcRenderer.invoke("hotkeys:list");
        HK_ITEMS = r.items || []; HK_REPORT = r.report || []; HK_STORE = r.store || HK_STORE;
        renderHotkeys();
      } catch (e) { box.textContent = "读取热键失败：" + e; }
    }

    function renderHotkeys() {
      const box = $("hkList");
      if (!box) return;
      if (!HK_ITEMS.length) {
        box.innerHTML = '<div class="box">未在任何插件 manifest.json 中发现 hotkeys 声明。</div>';
        $("hkResult").textContent = hkReportText();
        return;
      }
      box.innerHTML = HK_ITEMS.map(it => {
        const rep = HK_REPORT.find(x => x.key === it.key) || {};
        return '<div class="hk-row">' +
          '<div class="hk-meta"><div class="hk-name">' + hkEsc(it.name) + ' · ' + hkEsc(it.pluginName) + '</div>' +
          '<div class="hk-sub">' + hkEsc(it.desc || it.method || it.action || "") +
            ' · ' + hkEsc(it.scope) + (it.overridden ? " · 已自定义" : "") +
            (rep.error ? ' · <span style="color:var(--cinnabar)">' + hkEsc(rep.error) + '</span>' : "") + '</div></div>' +
          '<button class="hk-combo' + (it.combo ? "" : " rec") + '" data-hkcombo="' + hkEsc(it.key) + '" title="点击后按下新组合键；Esc 取消">' + hkEsc(it.combo || "点击录制") + '</button>' +
          '<label class="lc-toggle" style="margin:0"><input type="checkbox" data-hken="' + hkEsc(it.key) + '"' + (it.enabled ? " checked" : "") + '><span style="font-size:12px">启用</span></label>' +
          '<span class="hk-badge ' + (rep.status || "") + '">' + hkEsc(HK_BADGE[rep.status] || "未生效") + '</span>' +
          '<button class="mini outline" data-hkreset="' + hkEsc(it.key) + '">恢复默认</button>' +
        '</div>';
      }).join("");
      box.querySelectorAll("[data-hkcombo]").forEach(btn => { btn.onclick = () => startHkCapture(btn); });
      box.querySelectorAll("[data-hken]").forEach(cb => { cb.onchange = () => hkSave(cb.dataset.hken, undefined, cb.checked); });
      box.querySelectorAll("[data-hkreset]").forEach(b => {
        b.onclick = async () => {
          const r = await ipcRenderer.invoke("hotkeys:reset", { key: b.dataset.hkreset });
          if (!r.ok) { $("hkResult").textContent = "恢复默认失败：" + r.error; return; }
          HK_ITEMS = r.items || []; HK_REPORT = r.report || []; renderHotkeys();
        };
      });
      $("hkResult").textContent = hkReportText();
    }

    async function hkSave(key, combo, enabled = undefined) {
      const r = await ipcRenderer.invoke("hotkeys:set", { key, combo, enabled });
      if (!r.ok) { $("hkResult").textContent = "保存失败：" + r.error; renderHotkeys(); return; }
      HK_ITEMS = r.items || []; HK_REPORT = r.report || []; renderHotkeys();
    }

    async function startHkCapture(btn) {
      if (hkRec) return;
      hkRec = { key: btn.dataset.hkcombo, btn };
      btn.classList.add("rec"); btn.textContent = "按下组合键…";
      await ipcRenderer.invoke("hotkeys:captureStart"); // 先注销全部，避免按到已注册组合时直接触发动作
    }
    async function endHkCapture(combo) {
      const rec = hkRec; if (!rec) return;
      hkRec = null; rec.btn.classList.remove("rec");
      if (combo) { await hkSave(rec.key, combo); return; }
      const r = await ipcRenderer.invoke("hotkeys:captureEnd");
      if (r && r.items) { HK_ITEMS = r.items; HK_REPORT = r.report || []; }
      renderHotkeys();
    }
    // 录制态：捕获阶段拦截按键，避免被命令面板等其它监听器吃掉
    window.addEventListener("keydown", (e) => {
      if (!hkRec) return;
      e.preventDefault(); e.stopPropagation();
      if (e.key === "Escape") { endHkCapture(""); return; }
      const c = comboFromEvent(e);
      if (c) endHkCapture(c);
    }, true);
    // 录制中途失焦/关窗 → 取消录制并恢复热键注册（否则全局键会一直处于挂起态）
    window.addEventListener("blur", () => { if (hkRec) endHkCapture(""); });
    window.addEventListener("beforeunload", () => { if (hkRec) ipcRenderer.invoke("hotkeys:captureEnd"); });

    $("btnHkRefresh").onclick = () => refreshHotkeys();
    $("btnHkResetAll").onclick = async () => {
      if (!confirm("将清除全部热键自定义（改键与禁用），恢复各插件 manifest.json 的默认声明，继续？")) return;
      const r = await ipcRenderer.invoke("hotkeys:reset", {});
      if (!r.ok) { $("hkResult").textContent = "恢复失败：" + r.error; return; }
      HK_ITEMS = r.items || []; HK_REPORT = r.report || []; renderHotkeys();
    };

    // 应用内（app 作用域）热键：主进程不注册，由渲染进程在窗口聚焦时匹配派发。
    let HK_APP_CACHE = [];
    async function loadHotkeyCache() {
      try {
        const r = await ipcRenderer.invoke("hotkeys:list");
        HK_ITEMS = r.items || []; HK_REPORT = r.report || []; HK_APP_CACHE = HK_ITEMS;
      } catch (e) { /* 主进程未就绪时忽略 */ }
    }
    window.addEventListener("keydown", (e) => {
      if (hkRec || IS_SETTINGS) return;
      const combo = comboFromEvent(e);
      if (!combo) return;
      const hit = HK_APP_CACHE.find(h => h.scope === "app" && h.enabled && h.combo === combo);
      if (!hit) return;
      e.preventDefault();
      if (hit.method) rpc("plugin.call", { pluginId: hit.pluginId, method: hit.method, params: hit.params || {} }).catch(() => {});
    });
    // 热键触发反馈：状态栏提示（未聚焦触发时也能看到结果）
    ipcRenderer.on("hotkey:fired", (e, d) => setStatus("热键 " + (d.combo || "") + " · " + (d.name || ""), "ok"));
    ipcRenderer.on("hotkey:error", (e, d) => setStatus("热键失败 · " + (d.name || "") + "：" + d.error, "err"));

    // ── 命令面板（阶段D，FR-9） ──
    const SYS_CMDS = [
      { name: "/goto:settings", desc: "打开「设置」面板", kind: "sys", act: () => switchPanel("settings") },
    ];
    let PALETTE = [];     // 全部命令（sys + 内核插件命令）
    let FILTERED = [];
    let selIdx = 0;
    function switchPanel(name) { showPanel(name); }
    function refreshPalette() {
      return rpc("command.list", {}).then(r => {
        PALETTE = SYS_CMDS.concat((r.commands || []).map(c => ({
          name: c.name, desc: c.desc, kind: "plugin", pluginId: c.pluginId,
          method: c.method, params: c.params || [],
          act: async () => {
            const { ok } = await rpc("plugin.call", { pluginId: c.pluginId, method: c.method, params: {} });
            return JSON.stringify(ok);
          },
        })));
        renderPalette();
      });
    }
    function renderPalette() {
      const q = $("paletteInput").value.trim().toLowerCase();
      FILTERED = PALETTE.filter(p => !q || p.name.toLowerCase().includes(q) || (p.desc || "").toLowerCase().includes(q));
      selIdx = 0;
      const list = $("paletteList");
      if (!FILTERED.length) { list.innerHTML = `<div class="pal-empty">无匹配命令</div>`; return; }
      list.innerHTML = FILTERED.map((p, i) =>
        `<div class="pal-item${i === selIdx ? " sel" : ""}" data-i="${i}">
          <span class="n">${p.name}</span>
          ${p.params.length ? `<span class="p">·${p.params.length} 参</span>` : ""}
          <span class="s">${p.kind === "sys" ? "系统" : (p.pluginId || "")}</span>
          <span class="d">${p.desc || ""}</span>
        </div>`).join("");
    }
    function selectDelta(d) {
      selIdx = (selIdx + d + FILTERED.length) % FILTERED.length;
      const els = $("paletteList").querySelectorAll(".pal-item");
      els.forEach(el => el.classList.toggle("sel", +el.dataset.i === selIdx));
      els[selIdx] && els[selIdx].scrollIntoView({ block: "nearest" });
    }
    async function execPalette() {
      const cmd = FILTERED[selIdx];
      if (!cmd) return;
      closePalette();
      try {
        const out = await cmd.act();
        console.log(`[palette] ${cmd.name} →`, out);
      } catch (e) { console.error(`[palette] ${cmd.name} 执行失败`, e.message); }
    }
    function openPalette() { $("palette").classList.add("open"); $("paletteInput").value = ""; refreshPalette(); $("paletteInput").focus(); }
    function closePalette() { $("palette").classList.remove("open"); }
    ipcRenderer.on("palette:open", openPalette);
    $("paletteInput").addEventListener("input", renderPalette);
    $("paletteInput").addEventListener("keydown", (e) => {
      if (e.key === "Escape") closePalette();
      else if (e.key === "ArrowDown") { e.preventDefault(); selectDelta(1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); selectDelta(-1); }
      else if (e.key === "Enter") { e.preventDefault(); execPalette(); }
    });
    $("paletteList").addEventListener("mousedown", (e) => {
      const it = e.target.closest(".pal-item"); if (!it) return;
      selIdx = +it.dataset.i; execPalette();
    });
    $("palette").addEventListener("mousedown", (e) => { if (e.target === $("palette")) closePalette(); });
  