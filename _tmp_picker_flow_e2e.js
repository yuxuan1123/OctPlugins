// 临时 E2E：验证 fireTranslateRealtime 编排所依赖的后端状态契约。
// pick_reset → pick_begin(pickerActive) → set_region → pick_done(pickerDone)
// → start screenSub(subRunning) → stop realtime(subRunning=false)。
const { spawn } = require("child_process");
const WebSocket = require("./ui/node_modules/ws");

const root = "d:/Project/ElectronProject/OctPlugins";
const kp = spawn(root + "/kernel/kerneld.exe", [], { cwd: root, windowsHide: true });

let buf = "";
const bootTimer = setTimeout(() => { console.error("AUTH TIMEOUT"); process.exit(1); }, 60000);

kp.stdout.on("data", (d) => {
  buf += d.toString();
  const i = buf.indexOf("\n");
  if (i < 0) return;
  const line = buf.slice(0, i).trim();
  const { auth } = JSON.parse(line);
  clearTimeout(bootTimer);
  run(auth).catch(e => { console.error("RUN ERR", e); process.exit(1); });
});
kp.stderr.on("data", d => process.stderr.write("[kerneld] " + d));

let seq = 0;
function rpc(ws, method, params, timeoutMs = 60000) {
  return new Promise((resolve, reject) => {
    const id = ++seq;
    const t = setTimeout(() => reject(new Error(method + " timeout")), timeoutMs);
    const onMsg = (m) => {
      const msg = JSON.parse(m.toString());
      if (msg.id !== id) return;
      ws.off("message", onMsg);
      clearTimeout(t);
      msg.error ? reject(Object.assign(new Error(msg.error.message), { data: msg.error.data })) : resolve(msg.result);
    };
    ws.on("message", onMsg);
    ws.send(JSON.stringify({ v: 1, jsonrpc: "2.0", id, method, params }));
  });
}
const callT = (ws, method, params) => rpc(ws, "plugin.call", { pluginId: "translate", method, params: params || {} });
const unwrap = (r) => r && r.result !== undefined ? r.result : r;

let failures = 0;
function check(name, cond, extra) {
  console.log((cond ? "PASS" : "FAIL") + "  " + name + (extra ? "  " + extra : ""));
  if (!cond) failures++;
}

async function run(auth) {
  const ws = new WebSocket("ws://127.0.0.1:" + auth.port);
  await new Promise((res, rej) => { ws.on("open", res); ws.on("error", rej); });
  await rpc(ws, "kernel.hello", { client: "octplugin-host", token: auth.token });

  // 1) pick_reset 清场
  await callT(ws, "translate.app.pick_reset", {});
  let st = unwrap(await callT(ws, "translate.app.status", {}));
  check("pick_reset 后 pickerActive/Done/Abort 全 false",
    !st.pickerActive && !st.pickerDone && !st.pickerAbort, JSON.stringify({ a: st.pickerActive, d: st.pickerDone, ab: st.pickerAbort }));

  // 2) pick_begin → pickerActive
  await callT(ws, "translate.app.pick_begin", {});
  st = unwrap(await callT(ws, "translate.app.status", {}));
  check("pick_begin 后 pickerActive=true", !!st.pickerActive);

  // 3) set_region（写一块有效区域）
  const region = { x: 200, y: 200, w: 500, h: 260 };
  await callT(ws, "translate.app.set_region", { region });
  const rg = unwrap(await callT(ws, "translate.app.region", {}));
  check("set_region 后 region 读回一致 + have=true",
    rg.have && rg.region && rg.region.x === 200 && rg.region.y === 200 && rg.region.w === 500 && rg.region.h === 260,
    JSON.stringify(rg.region || {}));

  // 4) pick_done → pickerDone=true（编排据此进入开窗）
  await callT(ws, "translate.app.pick_done", {});
  st = unwrap(await callT(ws, "translate.app.status", {}));
  check("pick_done 后 pickerDone=true", !!st.pickerDone);

  // 5) start screenSub → subRunning=true
  const started = await callT(ws, "translate.app.start", { key: "screenSub", intervalMs: 1200 });
  const startedInner = unwrap(started);
  check("translate.app.start(screenSub) 正常返回", !!startedInner && startedInner.ok !== false, JSON.stringify(startedInner).slice(0, 200));
  st = unwrap(await callT(ws, "translate.app.status", {}));
  check("start 后 subRunning=true", !!st.subRunning);

  // 6) 字幕循环一拍：app.sub 返回结构含 running
  await new Promise(r => setTimeout(r, 1500));
  const sub = unwrap(await callT(ws, "translate.app.sub", {}));
  check("app.sub 返回 running=true", !!sub.running, JSON.stringify({ running: sub.running, paused: sub.paused }));

  // 7) stop realtime（热键停止分支用的 key）→ subRunning=false
  await callT(ws, "translate.app.stop", { key: "realtime" });
  st = unwrap(await callT(ws, "translate.app.status", {}));
  check("stop(realtime) 后 subRunning=false", !st.subRunning);

  // 8) abort 路径：pick_begin → pick_abort → pickerAbort=true
  await callT(ws, "translate.app.pick_reset", {});
  await callT(ws, "translate.app.pick_begin", {});
  await callT(ws, "translate.app.pick_abort", {});
  st = unwrap(await callT(ws, "translate.app.status", {}));
  check("pick_abort 后 pickerAbort=true", !!st.pickerAbort);

  ws.close();
  kp.kill();
  console.log(failures ? "\n=== " + failures + " 项失败 ===" : "\n=== 全部通过 ===");
  process.exit(failures ? 1 : 0);
}
