// 临时 E2E：完全复现热键 fireHotkey → plugin.call translate.app.toggle 的调用。
const { spawn } = require("child_process");
const WebSocket = require("./ui/node_modules/ws");

const root = "d:/Project/ElectronProject/OctPlugins";
const kp = spawn(root + "/kernel/kerneld.exe", [], { cwd: root, windowsHide: true });

let buf = "";
let timer = setTimeout(() => { console.error("AUTH TIMEOUT"); process.exit(1); }, 60000);

kp.stdout.on("data", (d) => {
  buf += d.toString();
  const i = buf.indexOf("\n");
  if (i < 0) return;
  const line = buf.slice(0, i).trim();
  const { auth } = JSON.parse(line);
  clearTimeout(timer);
  run(auth).catch(e => { console.error("RUN ERR", e); process.exit(1); });
});
kp.stderr.on("data", d => process.stderr.write("[kerneld] " + d));

let seq = 0;
const wsRef = {};
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

async function run(auth) {
  const ws = new WebSocket("ws://127.0.0.1:" + auth.port);
  wsRef.ws = ws;
  await new Promise((res, rej) => { ws.on("open", res); ws.on("error", rej); });
  const hello = await rpc(ws, "kernel.hello", { client: "octplugin-host", token: auth.token });
  console.log("hello ok, kernel =", hello.kernel);

  const status0 = await rpc(ws, "plugin.call", { pluginId: "translate", method: "translate.app.status", params: {} });
  console.log("app.status =>", JSON.stringify(status0).slice(0, 500));

  console.log("\n=== 热键 Alt+X 等效调用: toggle key=one_shot ===");
  try {
    const r = await rpc(ws, "plugin.call",
      { pluginId: "translate", method: "translate.app.toggle", params: { key: "one_shot" }, timeoutMs: 60000 }, 60000);
    console.log("one_shot RESULT =>", JSON.stringify(r).slice(0, 1200));
  } catch (e) {
    console.log("one_shot ERROR =>", e.message, JSON.stringify(e.data || {}));
  }

  console.log("\n=== 热键 Alt+C 等效调用: toggle key=realtime ===");
  try {
    const r = await rpc(ws, "plugin.call",
      { pluginId: "translate", method: "translate.app.toggle", params: { key: "realtime" } });
    console.log("realtime RESULT =>", JSON.stringify(r));
  } catch (e) {
    console.log("realtime ERROR =>", e.message, JSON.stringify(e.data || {}));
  }
  await new Promise(r => setTimeout(r, 3500));
  const sub = await rpc(ws, "plugin.call", { pluginId: "translate", method: "translate.app.sub", params: {} });
  console.log("after 3.5s app.sub =>", JSON.stringify(sub).slice(0, 800));
  await rpc(ws, "plugin.call", { pluginId: "translate", method: "translate.app.stop", params: { key: "realtime" } });

  const logs = await rpc(ws, "plugin.call", { pluginId: "translate", method: "translate.logs", params: {} });
  console.log("\nplugin logs:\n" + ((logs.result && logs.result.logs) || logs.logs || []).slice(-25).join("\n"));

  ws.close();
  kp.kill();
  process.exit(0);
}
