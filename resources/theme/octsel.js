// 「搜索 + 选择」组合框 —— 宿主设置页与各插件页共用的唯一实现（精简版）。
//
// 引用方式（一份文件，四处复用）：
//   · 宿主设置页（file:// 文档）：<script src="build/octsel.js">，
//     该副本由 ui 的 npm run build 从本文件拷入 ui/build/（见 ui/package.json）。
//   · 插件 iframe（内核 HTTP 资源服务）：<script src="/res/theme/octsel.js">。
//
// 统一交互约定（宿主与插件完全一致）：
//   · 仅点击/聚焦输入框才展开选项；打开窗口、刷新列表不主动弹出；
//   · 鼠标脱离组合框（输入框 + ✕ + 选项框）2s 自动收回，移回框内即取消计时；
//   · 输入即过滤：命中项置顶加粗，未命中项仍全部展示（置灰），不会“消失找不到”；
//   · 「清空」✕ 仅在有内容时显示；点击后清空、收回下拉、保留焦点，便于继续输入或再点弹出；
//   · 支持 ↑/↓ 高亮、Enter 选中、Esc 收起。
//
// 两种装配方式，返回同一套 API：
//   OctSelect.attach(input, drop, opts) —— 页面已有 <input> 与下拉容器，就地接线；
//   OctSelect.mount(box, opts)          —— 由本组件在 box 内创建 input/✕/下拉。
// API：get()/set(v)/fill(items)/onChange(fn)（mount 用）、refresh()/close()/text()（attach 用）。
(function () {
  "use strict";
  if (window.OctSelect) return;

  // ── 样式：一次性注入。颜色令牌沿用各页面的主题变量（宿主 :root / 插件 plugin.css） ──
  (function injectCSS() {
    if (document.getElementById("oct-sel-css")) return;
    var st = document.createElement("style");
    st.id = "oct-sel-css";
    st.textContent =
      ".oct-sel{position:relative}" +
      ".oct-sel-in{position:relative;display:block;width:100%}" +
      ".oct-sel-in>input{width:100%;box-sizing:border-box;background:rgba(255,252,245,.5);" +
        "border:1px solid var(--line);border-radius:10px;padding:9px 32px 9px 12px;color:var(--ink);" +
        "font-size:13px;font-family:inherit}" +
      ".oct-sel-in>input:focus{outline:none;border-color:var(--ink-soft)}" +
      ".oct-sel-clear{position:absolute;right:6px;top:50%;transform:translateY(-50%);width:22px;height:22px;" +
        "display:none;align-items:center;justify-content:center;margin:0;padding:0;border:none;" +
        "background:transparent;border-radius:50%;color:var(--ink-faint);font-family:inherit;font-size:13px;" +
        "line-height:1;letter-spacing:0;cursor:pointer}" +
      ".oct-sel-clear.show{display:flex}" +
      ".oct-sel-clear:hover{color:var(--cinnabar);background:rgba(178,58,48,.10)}" +
      ".oct-sel-drop{position:absolute;top:calc(100% + 4px);left:0;right:0;z-index:20;max-height:260px;" +
        "overflow-y:auto;background:var(--paper-2);border:1px solid var(--card-line);border-radius:10px;" +
        "box-shadow:var(--shadow)}" +
      ".oct-sel-item{padding:8px 12px;font-size:13px;color:var(--ink);cursor:pointer;" +
        "border-bottom:1px solid var(--card-line)}" +
      ".oct-sel-item:last-child{border-bottom:none}" +
      ".oct-sel-item:hover,.oct-sel-item.cur{background:rgba(178,58,48,.10);color:var(--cinnabar)}" +
      ".oct-sel-item.hit{font-weight:700;color:var(--ink)}" +
      ".oct-sel-item:not(.hit){color:var(--ink-faint)}";
    document.head.appendChild(st);
  })();

  var registry = []; // 已装配实例：一个 document 监听器统一处理「点击外部收起」

  function build(input, drop, opts) {
    var onPick = opts.onPick || null;       // 选中某项：onPick(item)
    var onCommit = opts.onCommit || null;   // 文本提交：onCommit(input.value)
    var onClear = opts.onClear || null;     // 清空：onClear()
    var changeCb = null;                    // mount 模式的统一回调：changeCb(当前值)，选中与清空都会触发
    var commitOn = opts.commitOn || ["change", "blur", "enter"];
    var matchFn = opts.match || function (it, q) { return !q || it.label.toLowerCase().indexOf(q) >= 0; };
    var normalize = !!opts.normalizeOnBlur; // 失焦时把输入回填为选中项文案（插件模式开启）
    var filled = [];                        // fill() 提供的静态列表
    var val = "";
    var cur = -1;
    var timer = null;

    // ✕ 与输入框收进相对定位容器：✕ 与下拉都以输入框为基准，
    // 宿主 .lc-pick 内可能还有「刷新」按钮，不能被算进定位基准或悬停区。
    var wrap = document.createElement("div");
    wrap.className = "oct-sel-in";
    input.parentNode.insertBefore(wrap, input);
    wrap.appendChild(input);
    if (drop) wrap.appendChild(drop);
    var clr = document.createElement("button");
    clr.type = "button";
    clr.className = "oct-sel-clear";
    clr.textContent = "✕";
    clr.title = "清空";
    wrap.appendChild(clr);

    function items() { return typeof opts.items === "function" ? (opts.items() || []) : filled; }
    function labelOf(v) {
      var all = items();
      for (var i = 0; i < all.length; i++) if (all[i].v === v) return all[i].label;
      return v;
    }
    function sync() { clr.classList.toggle("show", !!input.value); }
    function clearT() { if (timer) { clearTimeout(timer); timer = null; } }
    function armT() {
      if (!drop || drop.hidden) return;
      clearT();
      timer = setTimeout(function () { timer = null; drop.hidden = true; cur = -1; }, 2000);
    }
    function close() { if (drop) drop.hidden = true; cur = -1; clearT(); }
    function commit() { if (onCommit) onCommit(input.value); }
    function mark() {
      var els = drop.querySelectorAll(".oct-sel-item");
      for (var i = 0; i < els.length; i++) els[i].classList.toggle("cur", i === cur);
    }
    function refresh() {
      sync();
      if (!drop) return;
      var q = (input.value || "").trim().toLowerCase();
      var all = items();
      var hit = [], rest = [];
      for (var i = 0; i < all.length; i++) {
        if (q && !matchFn(all[i], q)) rest.push(all[i]); else hit.push(all[i]);
      }
      var list = hit.concat(rest);
      var show = document.activeElement === input && list.length > 0;
      drop.hidden = !show;
      if (!show) { clearT(); return; }
      drop.innerHTML = "";
      list.forEach(function (it, i) {
        var d = document.createElement("div");
        d.className = "oct-sel-item" + (q && i < hit.length ? " hit" : "");
        d.textContent = it.label;
        d.onmousedown = function (ev) { ev.preventDefault(); pick(it); };
        drop.appendChild(d);
      });
      // 键盘聚焦时鼠标可能不在框内：立即起 2s 收回计时
      if (!wrap.matches(":hover")) armT();
    }
    function pick(it) {
      val = it.v;
      input.value = it.label;
      sync();
      close();
      if (changeCb) changeCb(val); else if (onPick) onPick(it);
    }
    function pickCur() {
      var el = drop && drop.children[cur];
      if (!el) return false;
      var all = items();
      for (var i = 0; i < all.length; i++) if (all[i].label === el.textContent) { pick(all[i]); return true; }
      return false;
    }
    function pickExact() {
      var t = (input.value || "").trim().toLowerCase();
      var all = items();
      for (var i = 0; i < all.length; i++) if (all[i].label.toLowerCase() === t) { pick(all[i]); return true; }
      return false;
    }

    input.addEventListener("focus", function () { cur = -1; refresh(); });
    // 已聚焦时再单击不再触发 focus：补 click，保证「焦点在但列表已收起」时点一下也能弹出列表
    input.addEventListener("click", function () { cur = -1; refresh(); });
    input.addEventListener("input", function () { cur = -1; refresh(); });
    input.addEventListener("change", function () { if (commitOn.indexOf("change") >= 0) commit(); });
    input.addEventListener("blur", function () {
      if (commitOn.indexOf("blur") >= 0) commit();
      setTimeout(function () {
        close();
        if (normalize && input.value !== labelOf(val)) input.value = val ? labelOf(val) : "";
        sync();
      }, 120);
    });
    input.addEventListener("keydown", function (e) {
      if (e.key === "ArrowDown") {
        if (drop && !drop.hidden) { e.preventDefault(); cur = Math.min(cur + 1, drop.children.length - 1); mark(); }
        return;
      }
      if (e.key === "ArrowUp") {
        if (drop && !drop.hidden) { e.preventDefault(); cur = Math.max(cur - 1, 0); mark(); }
        return;
      }
      if (e.key === "Enter") {
        if (drop && !drop.hidden && cur >= 0 && pickCur()) { e.preventDefault(); return; }
        if (pickExact()) { e.preventDefault(); return; }
        if (commitOn.indexOf("enter") >= 0) commit();
        return;
      }
      if (e.key === "Escape") { close(); return; }
    });
    // 清空 ✕：mousedown 抢先 preventDefault，避免输入框先 blur（连带 120ms 收起）吞掉本次点击
    clr.addEventListener("mousedown", function (e) { e.preventDefault(); });
    clr.onclick = function () {
      input.value = "";
      val = "";
      sync();
      if (changeCb) changeCb(""); else if (onClear) onClear();
      input.focus(); // 保留焦点，便于继续输入
      // 收起须放在 focus 之后：否则 focus 触发的 refresh 会因「已聚焦且为空」又把它展开
      close();
    };
    wrap.addEventListener("mouseenter", clearT);
    wrap.addEventListener("mouseleave", armT);

    registry.push({ wrap: wrap, close: close });
    sync();
    return {
      el: wrap,
      input: input,
      get: function () { return val; },
      set: function (v) { val = v || ""; input.value = val ? labelOf(val) : ""; sync(); },
      fill: function (its) {
        filled = its || [];
        if (val && !filled.some(function (x) { return x.v === val; })) val = "";
        input.value = val ? labelOf(val) : "";
        sync();
      },
      onChange: function (fn) { changeCb = fn; },
      refresh: refresh,
      close: close,
      text: function () { return input.value; },
    };
  }

  window.OctSelect = {
    // 就地接线：页面已有 <input> 与下拉容器（宿主设置页用）
    attach: function (input, drop, opts) { return build(input, drop, opts || {}); },
    // 由组件创建控件：box 内自动生成 input / ✕ / 下拉（插件页用）
    mount: function (box, opts) {
      opts = opts || {};
      box.classList.add("oct-sel");
      var input = document.createElement("input");
      input.type = "text";
      input.placeholder = opts.placeholder || "";
      input.autocomplete = "off";
      var drop = document.createElement("div");
      drop.className = "oct-sel-drop";
      drop.hidden = true;
      box.appendChild(input);
      box.appendChild(drop);
      return build(input, drop, Object.assign({ normalizeOnBlur: true, commitOn: [] }, opts));
    },
  };

  // 点击组合框外部统一收起（一个监听器覆盖所有实例）
  document.addEventListener("click", function (ev) {
    for (var i = 0; i < registry.length; i++) {
      if (!registry[i].wrap.contains(ev.target)) registry[i].close();
    }
  });
})();