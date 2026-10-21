// 内嵌浏览器面板（browser 插件）：运行在宿主主窗渲染进程里。
// 为什么不做成 iframe：Electron 的 <webview> 不能在 iframe 内创建，
// 所以标签栏、地址栏与页面视图必须与宿主同处一个渲染进程。
// 与主进程的契约（ui/src/main.ts）：
//   browser:tab-open / browser:open-tab / browser:shortcut / browser:close-tab  ← 主进程下发
//   browser:attach（invoke）/ browser:tab-ready（send）                          → 主进程
//
// 性能模型（本次重构的核心）：标签是「元数据」，webview 是「可丢弃的载体」。
//   - lazy：后台标签只建标签、不建 webview（启动时 3 个飞书只真正加载 1 个）；
//   - discard：后台标签闲置到阈值就把 webview 从 DOM 移除（guest 进程随之销毁），
//     只保留 url/title/favicon，切回时按 url 重建；
//   - 上限：标签数超过 maxTabs 时，按最久未访问回收（任务标签未回传时不动）；
//   - 任务标签：加载回传后若一直不在前台，taskCloseMs 后自动关闭；
//   - 面板冷启动：切到别的插件超过 panelColdMs，卸载全部标签并彻底 display:none。
(() => {
  "use strict";

  const { ipcRenderer } = require("electron");

  const panelEl = document.getElementById("panel-plugin.browser");
  if (!panelEl) return;

  const $ = (id) => document.getElementById(id);
  const stripEl = $("bTabstrip");
  const viewport = $("bViewport");
  const addr = $("bAddr");
  const startEl = $("bStart");
  const startInput = $("bStartInput");
  const btnBack = $("bBack");
  const btnFwd = $("bFwd");
  const btnReload = $("bReload");
  const btnHome = $("bHome");
  const progressEl = $("bProgress");

  const PARTITION = "persist:oct-browser"; // 与旧独立窗口共用同一分区，登录态可复用
  const safe = (fn, dflt) => { try { return fn(); } catch (e) { return dflt; } };

  // 回收策略（单位 ms）。idleDiscardMs 保守取值：用户可能正在后台标签里编辑文档，
  // 丢弃会丢未保存内容，所以只回收「15 分钟没被访问过」的后台标签。
  const CFG = {
    maxTabs: 12,
    idleDiscardMs: 15 * 60 * 1000,
    panelColdMs: 10 * 60 * 1000,
    taskCloseMs: 60 * 1000,
  };

  const chromeTabs = new ChromeTabs();
  chromeTabs.init(stripEl);

  // tab: { id, wv|null, el, title, url, favicon, taskId, notified, loading, wcId,
  //        lastActive, idleTimer, discarded, crashed }
  const tabs = new Map();
  let activeId = 0;
  let seq = 0;
  let addrEditing = false;
  let booted = false;
  let panelTimer = 0;

  const isActive = () => panelEl.classList.contains("active");
  const isBlank = (t) => !t || !t.url || t.url === "about:blank";

  // ── 地址栏解析：像网址就补协议，否则走搜索 ──────────────────────────
  function normalizeInput(s) {
    s = String(s || "").trim();
    if (!s) return "";
    if (/^[a-z][a-z0-9+.-]*:/i.test(s)) return s;
    if (/^(localhost|127\.0\.0\.1)(:\d+)?(\/|$)/.test(s)) return "http://" + s;
    if (/^[\w-]+(\.[\w-]+)+(:\d+)?([/?#]|$)/.test(s) && !/\s/.test(s)) return "https://" + s;
    return "https://www.bing.com/search?q=" + encodeURIComponent(s);
  }

  // 懒加载标签先给个能认出来的标题，避免一排「新标签页」。
  function labelOf(url) {
    try { return new URL(url).hostname || "新标签页"; } catch (e) { return "新标签页"; }
  }

  function setAddr(url) {
    addr.value = (!url || url === "about:blank") ? "" : url;
  }

  // ── 标签生命周期 ───────────────────────────────────────────────────
  function createTab(url, opts = {}) {
    const id = ++seq;
    const activate = opts.activate !== false;
    const target = url || "about:blank";
    // 空白页与显式 lazy 的标签不建 webview：前者省一个空进程，后者等切到才加载。
    const lazy = opts.lazy === true || target === "about:blank";

    const tab = {
      id, wv: null, el: null,
      title: lazy && target !== "about:blank" ? labelOf(target) : "新标签页",
      url: target,
      favicon: "",
      taskId: opts.taskId || "",
      notified: false,
      loading: false,
      wcId: 0,
      lastActive: Date.now(),
      idleTimer: 0,
      discarded: lazy && target !== "about:blank",
      crashed: false,
    };
    tabs.set(id, tab);            // 必须先入 Map：addTab 会同步触发 activeTabChange
    chromeTabs.addTab({ title: tab.title, id: String(id) }, { background: !activate });
    tab.el = chromeTabs.tabEls.find((el) => el.dataset.tabId === String(id)) || null;
    if (!lazy) ensureLive(tab);   // 激活的标签由 showTab 建，这里兜住后台非 lazy 的情况
    else updateTabVisual(tab);
    enforceCap();
    return tab;
  }

  // 按需创建 webview（休眠标签被激活、或导航到新地址时调用）。
  function ensureLive(tab) {
    if (!tab) return null;
    if (tab.wv) return tab.wv;
    if (isBlank(tab)) return null; // 空白页只显示起始页，不占进程
    // 面板若处于 is-cold（display:none），webview 在隐藏子树里可能一直不 attach，
    // 后台任务就永远等不到 did-finish-load。这里只恢复 display，保持 visibility:hidden
    // （仍然不绘制、不抢焦点），并重新开始冷启动计时。
    if (panelEl.classList.contains("is-cold")) {
      panelEl.classList.remove("is-cold");
      if (!isActive()) armPanelCold();
    }
    const wv = document.createElement("webview");
    wv.className = "b-page" + (tab.id === activeId ? "" : " hidden");
    wv.setAttribute("partition", PARTITION);
    wv.setAttribute("allowpopups", "");
    wv.setAttribute("src", tab.url);
    tab.wv = wv;
    tab.discarded = false;
    tab.crashed = false;
    tab.wcId = 0;
    viewport.appendChild(wv);
    bindTab(tab, wv);
    updateTabVisual(tab);
    return wv;
  }

  // 卸载载体：webview 移出 DOM 即销毁 guest 进程，只留元数据。
  function discardTab(tab) {
    if (!tab) return;
    clearIdle(tab);
    const wv = tab.wv;
    if (!wv) return;
    const url = safe(() => wv.getURL(), "") || tab.url;
    tab.wv = null;
    tab.wcId = 0;
    tab.loading = false;
    tab.discarded = true;
    if (url && url !== "about:blank") tab.url = url;
    safe(() => wv.remove());
    updateTabVisual(tab);
    if (tab.id === activeId) { updateToolbar(); updateProgress(); addr.classList.remove("loading"); }
  }

  function bindTab(tab, wv) {
    // 所有回调都校验 wv 身份：丢弃后重建的 webview 不能让旧实例的迟到事件改状态。
    const alive = () => tab.wv === wv;

    wv.addEventListener("dom-ready", () => {
      if (!alive()) return;
      tab.wcId = safe(() => wv.getWebContentsId(), 0);
      if (tab.wcId) {
        safe(() => ipcRenderer.invoke("browser:attach", { tabId: tab.id, webContentsId: tab.wcId }));
      }
      if (tab.id === activeId) safe(() => wv.focus());
    });

    // 定时/外部任务：加载完成回传宿主（插件据此写日志）。普通浏览无 taskId，不走这里。
    wv.addEventListener("did-finish-load", () => {
      if (!alive()) return;
      tab.crashed = false;
      if (!tab.taskId || tab.notified) return;
      tab.notified = true;
      try {
        ipcRenderer.send("browser:tab-ready", { taskId: tab.taskId, tabId: tab.id, webContentsId: tab.wcId });
      } catch (e) { /* ignore */ }
      if (tab.id !== activeId) armIdle(tab); // 任务标签不在前台 → 起回收计时
    });

    wv.addEventListener("did-start-loading", () => {
      if (!alive()) return;
      tab.loading = true;
      if (tab.id === activeId) { updateProgress(); addr.classList.add("loading"); }
    });

    wv.addEventListener("did-stop-loading", () => {
      if (!alive()) return;
      tab.loading = false;
      tab.url = safe(() => wv.getURL(), tab.url);
      if (tab.id === activeId) {
        addr.classList.remove("loading");
        syncAddr(); updateToolbar(); updateStart(); updateProgress();
      }
    });

    wv.addEventListener("did-navigate", (e) => {
      if (!alive()) return;
      tab.url = e.url;
      if (tab.id === activeId) { syncAddr(); updateStart(); }
    });

    wv.addEventListener("did-navigate-in-page", (e) => {
      if (!alive()) return;
      tab.url = e.url;
      if (tab.id === activeId) syncAddr();
    });

    wv.addEventListener("page-title-updated", (e) => {
      if (!alive()) return;
      tab.title = (e && e.title) || tab.title;
      updateTabVisual(tab);
    });

    wv.addEventListener("page-favicon-updated", (e) => {
      if (!alive()) return;
      const icon = e && e.favicons && e.favicons[0];
      if (!icon) return;
      tab.favicon = icon;
      updateTabVisual(tab);
    });

    wv.addEventListener("did-fail-load", (e) => {
      if (!alive()) return;
      if (!e || e.errorCode === -3) return; // -3 = ERR_ABORTED（主动中止）
      if (e.isMainFrame === false) return;  // 子资源失败不算整页失败
      tab.loading = false;
      tab.title = "无法访问此网站";
      updateTabVisual(tab);
      if (tab.id === activeId) { addr.classList.remove("loading"); updateProgress(); }
      // 任务标签加载失败：不会再有 did-finish-load，这里放开回收，避免僵尸标签常驻。
      if (tab.taskId && !tab.notified) {
        tab.notified = true;
        if (tab.id !== activeId) armIdle(tab);
      }
    });

    // 崩溃不自动重建（避免崩溃循环）：先卸载释放进程，切回或点刷新时再重建。
    wv.addEventListener("crashed", () => {
      if (!alive()) return;
      tab.crashed = true;
      tab.title = "页面崩溃 · 点击刷新";
      discardTab(tab);
      updateTabVisual(tab);
    });
  }

  // chrome-tabs 的 updateTab 不传 favicon 会清空图标，所以始终一起传。
  function updateTabVisual(tab) {
    if (!tab || !tab.el) return;
    safe(() => chromeTabs.updateTab(tab.el, { title: tab.title, favicon: tab.favicon }));
    if (tab.discarded) tab.el.setAttribute("data-sleep", "1");
    else tab.el.removeAttribute("data-sleep");
  }

  // ── 回收计时 ───────────────────────────────────────────────────────
  function clearIdle(tab) {
    if (tab && tab.idleTimer) { clearTimeout(tab.idleTimer); tab.idleTimer = 0; }
  }

  function armIdle(tab) {
    if (!tab) return;
    if (tab.idleTimer) return; // 已有倒计时：不要因为切换别的标签而重置，否则永远等不到回收
    if (tab.id === activeId) return;
    const isTask = !!tab.taskId;
    if (isTask && !tab.notified) return; // 任务还在加载，别动
    if (!tab.wv && !isTask) return;
    tab.idleTimer = setTimeout(() => {
      tab.idleTimer = 0;
      if (tab.id === activeId) return;
      if (isTask) { closeTab(tab.id); return; } // 结果已回传，标签不再需要
      if (tab.wv) discardTab(tab);
    }, isTask ? CFG.taskCloseMs : CFG.idleDiscardMs);
  }

  function enforceCap() {
    if (tabs.size <= CFG.maxTabs) return;
    const victims = Array.from(tabs.values())
      .filter((t) => t.id !== activeId)
      .sort((a, b) => a.lastActive - b.lastActive);
    let over = tabs.size - CFG.maxTabs;
    for (const t of victims) {
      if (over <= 0) break;
      if (t.taskId && !t.notified) continue;
      closeTab(t.id);
      over--;
    }
  }

  // ── 视图切换 ───────────────────────────────────────────────────────
  function showTab(tab) {
    activeId = tab.id;
    tab.lastActive = Date.now();
    clearIdle(tab);
    if (!isBlank(tab)) ensureLive(tab); // 休眠标签在这里按 url 重建
    for (const t of tabs.values()) {
      if (t.wv) t.wv.classList.toggle("hidden", t.id !== tab.id);
      if (t.id !== tab.id) armIdle(t);
    }
    syncAddr();
    updateToolbar();
    updateStart();
    updateProgress();
    if (tab.wv) safe(() => tab.wv.focus());
    else if (!startEl.hidden) safe(() => startInput.focus());
  }

  function syncAddr() {
    if (addrEditing) return;
    const t = tabs.get(activeId);
    setAddr(t ? t.url : "");
  }

  function activateTab(id) {
    const t = tabs.get(id);
    if (!t || !t.el) return;
    if (chromeTabs.activeTabEl !== t.el) chromeTabs.setCurrentTab(t.el);
    else showTab(t);
  }

  function closeTab(id) {
    const t = tabs.get(id);
    if (!t || !t.el) return;
    chromeTabs.removeTab(t.el); // 由 tabRemove 事件销毁 webview
  }

  // ── chrome-tabs 事件 ───────────────────────────────────────────────
  stripEl.addEventListener("activeTabChange", ({ detail }) => {
    const id = Number(detail.tabEl.dataset.tabId || 0);
    const t = tabs.get(id);
    if (!t) return;
    t.el = detail.tabEl;
    showTab(t);
  });

  stripEl.addEventListener("tabRemove", ({ detail }) => {
    const id = Number(detail.tabEl.dataset.tabId || 0);
    const t = tabs.get(id);
    if (!t) return;
    tabs.delete(id);
    clearIdle(t);
    if (t.wv) { safe(() => t.wv.remove()); t.wv = null; }
    if (tabs.size === 0) createTab("about:blank");
  });

  // 双击标签栏空白时组件会自动 addTab 一个无业务数据的空标签：回收它，建真实标签。
  stripEl.addEventListener("tabAdd", ({ detail }) => {
    if (detail.tabEl.dataset.tabId) return;
    setTimeout(() => {
      if (detail.tabEl.parentNode) chromeTabs.removeTab(detail.tabEl);
      createTab("about:blank");
    }, 0);
  });

  // chrome-tabs 的「点击标签激活」是通过 Draggabilly 的 pointerDown 实现的；
  // 本环境没有引入拖拽库（它的 UMD 依赖 jquery，nodeIntegration 下会失败），
  // 所以这里补一层事件委托，保证点击标签能切换。
  stripEl.addEventListener("click", (e) => {
    const tabEl = e.target.closest && e.target.closest(".chrome-tab");
    if (!tabEl || !stripEl.contains(tabEl)) return;
    if (e.target.closest(".chrome-tab-close")) return; // 关闭按钮自带处理
    chromeTabs.setCurrentTab(tabEl);
  });

  // 中键关闭标签（Chrome 习惯）
  stripEl.addEventListener("auxclick", (e) => {
    if (e.button !== 1) return;
    const tabEl = e.target.closest && e.target.closest(".chrome-tab");
    if (tabEl && stripEl.contains(tabEl)) chromeTabs.removeTab(tabEl);
  });

  // ── 工具栏 ─────────────────────────────────────────────────────────
  function updateToolbar() {
    const t = tabs.get(activeId);
    btnBack.disabled = !t || !t.wv || !safe(() => t.wv.canGoBack(), false);
    btnFwd.disabled = !t || !t.wv || !safe(() => t.wv.canGoForward(), false);
  }

  function updateStart() {
    const t = tabs.get(activeId);
    startEl.hidden = !isBlank(t);
  }

  function updateProgress() {
    if (!progressEl) return;
    const t = tabs.get(activeId);
    progressEl.hidden = !(t && t.loading);
  }

  function navigate(input) {
    const url = normalizeInput(input);
    if (!url) return;
    addrEditing = false;
    addr.blur();
    const t = tabs.get(activeId);
    if (!t) { createTab(url); return; }
    const hadWv = !!t.wv;
    t.url = url;
    t.crashed = false;
    t.discarded = false;
    startEl.hidden = true;
    ensureLive(t);
    if (t.wv && hadWv) safe(() => t.wv.loadURL(url));
  }

  // 外部请求打开网址（定时任务 / 插件 browse）：若当前只有一个空白起始标签就复用它，
  // 避免“起始页一个标签 + 目标网址一个标签”的重复。
  function openUrl(url, opts = {}) {
    const cur = tabs.get(activeId);
    const blankOnly = cur && tabs.size === 1 && isBlank(cur) && !cur.taskId;
    if (blankOnly) {
      const hadWv = !!cur.wv;
      cur.url = url;
      cur.taskId = opts.taskId || "";
      cur.notified = false;
      cur.crashed = false;
      cur.discarded = false;
      startEl.hidden = true;
      ensureLive(cur); // 新建时 src 已经是目标地址，不必再 loadURL（否则会重复导航一次）
      if (cur.wv && hadWv) safe(() => cur.wv.loadURL(url));
      return cur;
    }
    return createTab(url, opts);
  }

  function handleAct(act) {
    const t = tabs.get(activeId);
    if (act === "new-tab") { createTab("about:blank"); addr.focus(); return; }
    if (act === "close-tab") { if (t) closeTab(t.id); return; }
    if (act === "focus-addr") { addr.focus(); addr.select(); return; }
    if (!t) return;
    if (act === "reload") {
      if (!t.wv) { // 休眠 / 崩溃后刷新：重建并重新导航
        t.crashed = false;
        ensureLive(t);
        if (t.wv) safe(() => t.wv.loadURL(t.url));
        return;
      }
      t.loading ? safe(() => t.wv.stop()) : safe(() => t.wv.reload());
    }
    else if (act === "back") safe(() => t.wv.goBack());
    else if (act === "forward") safe(() => t.wv.goForward());
  }

  // ── 绑定 ───────────────────────────────────────────────────────────
  $("bTabNew").onclick = () => { createTab("about:blank"); addr.focus(); };
  btnBack.onclick = () => handleAct("back");
  btnFwd.onclick = () => handleAct("forward");
  btnReload.onclick = () => handleAct("reload");
  btnHome.onclick = () => { createTab("about:blank"); addr.focus(); };

  addr.addEventListener("focus", () => { addrEditing = true; safe(() => addr.select()); });
  addr.addEventListener("blur", () => { addrEditing = false; });
  addr.addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); navigate(addr.value); }
    else if (e.key === "Escape") { addrEditing = false; syncAddr(); addr.blur(); }
  });

  $("bStartForm").addEventListener("submit", (e) => {
    e.preventDefault();
    navigate(startInput.value);
  });

  document.querySelectorAll("#bShortcuts a").forEach((a) => {
    a.addEventListener("click", (e) => {
      e.preventDefault();
      const url = a.dataset.url;
      if (!url) return;
      const t = tabs.get(activeId);
      if (t && isBlank(t)) {
        const hadWv = !!t.wv;
        t.url = url;
        t.discarded = false;
        startEl.hidden = true;
        ensureLive(t);
        if (t.wv && hadWv) safe(() => t.wv.loadURL(url));
      } else {
        createTab(url);
      }
    });
  });

  // 快捷键只在浏览器面板激活时生效，避免占用其它插件的按键。
  window.addEventListener("keydown", (e) => {
    if (!isActive()) return;
    const ctrl = e.ctrlKey || e.metaKey;
    const k = String(e.key || "").toLowerCase();
    if (ctrl && k === "t") { e.preventDefault(); handleAct("new-tab"); }
    else if (ctrl && k === "w") { e.preventDefault(); handleAct("close-tab"); }
    else if (ctrl && k === "l") { e.preventDefault(); handleAct("focus-addr"); }
    else if (e.key === "F5" || (ctrl && k === "r")) { e.preventDefault(); handleAct("reload"); }
    else if (e.altKey && e.key === "ArrowLeft") { e.preventDefault(); handleAct("back"); }
    else if (e.altKey && e.key === "ArrowRight") { e.preventDefault(); handleAct("forward"); }
  });

  // ── 主进程事件 ─────────────────────────────────────────────────────
  ipcRenderer.on("browser:tab-open", (_e, p) => {
    if (!p || !p.url) return;
    openUrl(p.url, { taskId: p.taskId, activate: !!p.activate });
  });

  ipcRenderer.on("browser:open-tab", (_e, p) => {
    if (p && p.url) createTab(p.url, { activate: p.activate !== false });
  });

  ipcRenderer.on("browser:shortcut", (_e, p) => {
    if (p && p.act && isActive()) handleAct(p.act);
  });

  // 主进程（或插件经 browser.close-tab 事件）要求回收标签。
  ipcRenderer.on("browser:close-tab", (_e, p) => {
    const q = p || {};
    if (q.tabId) { closeTab(Number(q.tabId)); return; }
    if (q.taskId) {
      for (const t of tabs.values()) {
        if (t.taskId === q.taskId) { closeTab(t.id); break; }
      }
    }
  });

  // ── 首次显示时再建标签 ─────────────────────────────────────────────
  // 面板隐藏时创建 webview，guest 可能迟迟不 attach；等面板第一次切到前台再建。
  function ensureBoot() {
    const first = !booted;
    booted = true;
    if (tabs.size === 0) createTab("about:blank");
    updateToolbar();
    updateStart();
    updateProgress();
    if (first) {
      setTimeout(() => addr.focus(), 60);
      openStartupPages();
    }
  }

  // 自启动：面板首次显示时向 browser 插件要清单（startup.list），逐条打开。
  // 本次会话只做一次；用户手动关掉后不会反复弹回，重启应用后会再次打开。
  // 只有第 1 条立即加载，其余建标签但不建 webview（切到才加载）——
  // 启动瞬间只拉起 1 个飞书进程，而不是 3 个。
  // 兜底清单与 plugins/browser/main.js 的 DEFAULT_STARTUP 保持一致：插件未就绪时也能开。
  const FALLBACK_STARTUP = [
    "https://dej4esdop1.feishu.cn/wiki/WEpJwuaZditGJAkDJG6cGB4knQQ",
    "https://dej4esdop1.feishu.cn/sheets/shtcnj4YBiK5miobFJmXzf6wNcf?sheet=VE8X5A",
    "https://dej4esdop1.feishu.cn/wiki/RXTcw0iDXiYKDyk6yZZcVIN1nid?table=tbl8UVBA5oiQDvIX&view=vewH6RqXnd",
  ];
  let startupDone = false;
  function openStartupPages() {
    if (startupDone) return;
    startupDone = true;
    const timer = new Promise((resolve) => setTimeout(() => resolve([]), 8000));
    const request = ipcRenderer.invoke("browser:startup-list").catch((e) => {
      console.warn("[browser] startup.list 调用失败，改用内置清单", e);
      return [];
    });
    Promise.race([request, timer])
      .then((urls) => {
        const list = (Array.isArray(urls) ? urls : []).filter((u) => /^https?:\/\//i.test(String(u || "")));
        const finalList = list.length ? list : FALLBACK_STARTUP;
        if (tabs.size === 0) createTab("about:blank");
        finalList.forEach((u, i) => openUrl(String(u), { activate: i === 0, lazy: i > 0 }));
      })
      .catch(() => { /* 保留空白标签 */ });
  }

  // ── 面板可见性：冷启动卸载 ─────────────────────────────────────────
  function armPanelCold() {
    if (panelTimer) clearTimeout(panelTimer);
    panelTimer = setTimeout(() => {
      panelTimer = 0;
      if (isActive()) return;
      // 还有任务标签在跑：不要进 is-cold（display:none 会让未 attach 的 guest 卡住），
      // 过一会儿再来一次；任务结束后自然会被回收。
      const pendingTask = Array.from(tabs.values()).some((t) => t.wv && t.taskId && !t.notified);
      if (pendingTask) { armPanelCold(); return; }
      // 面板长时间不在前台：全部标签卸载（guest 进程退出），随后 display:none。
      // 此时 DOM 里已没有 webview，切回来重建不会白屏。
      for (const t of Array.from(tabs.values())) if (t.wv) discardTab(t);
      panelEl.classList.add("is-cold");
    }, CFG.panelColdMs);
  }

  function onPanelClassChange() {
    const active = isActive();
    // 浏览器面板激活时关掉宿主的重合成特效（blend/backdrop-filter），见 index.html。
    document.body.classList.toggle("browser-active", active);

    if (active) {
      if (panelTimer) { clearTimeout(panelTimer); panelTimer = 0; }
      if (panelEl.classList.contains("is-cold")) panelEl.classList.remove("is-cold");
      ensureBoot();
      const t = tabs.get(activeId);
      if (t && !t.wv && !isBlank(t)) ensureLive(t);
      return;
    }

    if (panelEl.classList.contains("is-cold")) return; // 已彻底隐藏，不用再计时
    if (panelTimer) return;
    armPanelCold();
  }

  const mo = new MutationObserver(onPanelClassChange);
  mo.observe(panelEl, { attributes: true, attributeFilter: ["class"] });
  if (isActive()) onPanelClassChange();
})();
