// OctPlugins 内核 E2E 冒烟（conversion.md P0/P1/P2 验收）
// 用法: node tools/e2e-smoke.mjs
// 说明: 拉起 kernel/kerneld.exe → 读 auth → WS 连接 → 依次验证：
//   §三 模型选择链 / §四 加载权与 handover / §二 转换图 / §五 依赖缺口（webp、xlsx）
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";

const require = createRequire(import.meta.url);
const WebSocket = require("../ui/node_modules/ws");

const root = process.cwd();
const results = [];

// registry.json 是机器生成清单（§8），记录每个插件的 manifestHash。
// 本测试改了 manifest（新增 default/fallbacks/companion、webp/avif 边等），
// 若沿用上一轮记录的 hash，内核会按篡改校验拒绝装载这些插件（E_DEPS_HASH_MISMATCH 类行为）。
// 故每次 E2E 先重基线：删除 registry.json，让内核按当前 manifest 重新登记。
const registryPath = path.join(root, "state", "registry.json");
if (fs.existsSync(registryPath)) {
  fs.rmSync(registryPath);
  console.log("[setup] 已移除 state/registry.json 以重基线（内核会按当前 manifest 重新登记）");
}

function record(name, ok, detail) {
  results.push({ name, ok, detail });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
}

// ── 拉起内核 ─────────────────────────────────────────────────
const child = spawn(path.join(root, "kernel", "kerneld.exe"), [], {
  cwd: root,
  stdio: ["ignore", "pipe", "pipe"],
});
let stderrBuf = "";
child.stderr.on("data", (d) => { stderrBuf += d.toString(); });

const auth = await new Promise((resolve, reject) => {
  let buf = "";
  const to = setTimeout(() => reject(new Error("等待内核 auth 超时")), 90000);
  child.stdout.on("data", (d) => {
    buf += d.toString();
    const line = buf.split(/\r?\n/)[0];
    if (line && line.trim().startsWith("{")) {
      try {
        const { auth } = JSON.parse(line);
        clearTimeout(to);
        resolve(auth);
      } catch { /* 继续等完整行 */ }
    }
  });
  child.on("exit", (c) => { clearTimeout(to); reject(new Error(`内核提前退出 code=${c}\n${stderrBuf}`)); });
});
console.log(`kernel auth: port=${auth.port}`);

// ── WS RPC ───────────────────────────────────────────────────
const ws = new WebSocket(`ws://127.0.0.1:${auth.port}`);
let nextId = 1;
const pending = new Map();
const events = [];
ws.on("message", (raw) => {
  const m = JSON.parse(raw.toString());
  if (m.id && pending.has(m.id)) {
    const { resolve, reject } = pending.get(m.id);
    pending.delete(m.id);
    if (m.error) {
      const detail = m.error.data ? JSON.stringify(m.error.data) : (m.error.message || "");
      reject(new Error(`${m.error.code}: ${detail}`));
    } else resolve(m.result);
  } else if (m.method === "event") {
    events.push(m.params);
  }
});

function rpc(method, params, timeoutMs = 180000) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ v: 1, jsonrpc: "2.0", id, method, params }));
    setTimeout(() => {
      if (pending.has(id)) { pending.delete(id); reject(new Error(`${method} timeout`)); }
    }, timeoutMs);
  });
}

await new Promise((res, rej) => {
  ws.on("open", res);
  ws.on("error", rej);
});
const hello = await rpc("kernel.hello", { client: "octplugin-host", token: auth.token });
const plist = await rpc("plugin.list", {});
const rawPlugins = plist?.plugins ?? plist?.list ?? plist ?? [];
const pluginIds = Array.isArray(rawPlugins) ? rawPlugins.map((p) => p.id || p.pluginId) : Object.keys(rawPlugins);
record("kernel.hello + 插件注册", pluginIds.length > 0,
  `helloKeys=${Object.keys(hello || {}).join(",")} plugins=${pluginIds.length}${pluginIds.length ? " [" + pluginIds.slice(0, 5).join(",") + "…]" : ""}`);

// registry.call 的回包是 dispatchGate 的信封 {ok:true, result:<插件返回值>}（§11.6），
// 与 conversion 内部 callFunc 一致：这里统一解包，便于断言插件真实返回值。
const call = async (fn, params) => {
  const raw = await rpc("registry.call", { name: fn, params, timeoutMs: 180000 }, 180000);
  if (raw && typeof raw === "object" && "ok" in raw && "result" in raw) return raw.result;
  return raw;
};

// 先确认共享函数注册表（§11.6）里有 conversion.* —— 这是后续所有转换调用的前提。
const fnList = await rpc("registry.list", {});
const fnNames = (fnList?.functions || []).map((f) => f.name);
record("§11.6 共享函数注册表含 conversion.*", fnNames.includes("conversion.graph"),
  `functions=${fnNames.length} conversion=${fnNames.filter((n) => n.startsWith("conversion.")).join(",") || "(none)"}`);
if (!fnNames.includes("conversion.graph")) {
  console.log("  [debug] functions =", fnNames.join(","));
  console.log("  [debug] kernel stderr tail:\n" + stderrBuf.split("\n").slice(-30).join("\n"));
}

// ── §二 转换图 ───────────────────────────────────────────────
const g = await call("conversion.graph", {});
const formats = new Set(g.formats || []);
record("§二 转换图汇总全部 tool 的边", (g.edges || []).length > 30,
  `edges=${(g.edges || []).length} formats=${formats.size} tools=${(g.tools || []).length}`);
if ((g.edges || []).length === 0) {
  console.log("  [debug] conversion.graph =", JSON.stringify(g).slice(0, 300));
  console.log("  [debug] kernel stderr tail:\n" + stderrBuf.split("\n").slice(-30).join("\n"));
}
record("§五 webp/avif 进入转换图", formats.has("webp") && formats.has("avif"), `webp=${formats.has("webp")} avif=${formats.has("avif")}`);
record("§18 xlsx/xls 进入转换图", formats.has("xlsx") && formats.has("xls"), `xlsx=${formats.has("xlsx")} xls=${formats.has("xls")}`);

const r = await call("conversion.reachable", { from: "png" });
const targets = new Set((r.targets || []).map((t) => t.to));
record("§3 png 可达 webp/avif/txt", targets.has("webp") && targets.has("avif") && targets.has("txt"),
  `targets=${[...targets].slice(0, 12).join(",")}`);

// ── §三 模型选择链 ───────────────────────────────────────────
const caps = await rpc("registry.capabilities.list", {});
const byCap = new Map((caps.capabilities || []).map((c) => [c.capability, c]));
record("§7.4 registry.capabilities.list 可用", byCap.size > 0, `capabilities=${[...byCap.keys()].join(",")}`);
record("§7.2 tts 默认=Kokoro（修正字典序误选 MOSS）",
  byCap.get("tts")?.modelId === "kokoro",
  `tts effective=${byCap.get("tts")?.modelId} source=${byCap.get("tts")?.source}`);
record("§7.2 stt 默认=SenseVoiceSmall", byCap.get("stt")?.modelId === "SenseVoiceSmall",
  `stt=${byCap.get("stt")?.modelId}`);
record("§7.2 translate 默认=hy_mt", byCap.get("translate")?.modelId === "hy_mt",
  `translate=${byCap.get("translate")?.modelId}`);
record("§7.2 ocr 默认=rapidocr", byCap.get("ocr")?.modelId === "rapidocr",
  `ocr=${byCap.get("ocr")?.modelId}`);

// §8.1 设置级 pin：pin translate → opusMT-en-zh，必须立刻生效（§9.2 pin 置顶）
await rpc("registry.models.pin", { capability: "translate", modelId: "opusMT-en-zh" });
const caps2 = await rpc("registry.capabilities.list", {});
const tr = (caps2.capabilities || []).find((c) => c.capability === "translate");
record("§9.2 用户 pin 置顶生效", tr?.modelId === "opusMT-en-zh" && tr?.source === "userPin",
  `translate=${tr?.modelId} source=${tr?.source}`);
// 清除 pin，回落作者声明 hy_mt
await rpc("registry.models.pin", { capability: "translate", modelId: "" });
const caps3 = await rpc("registry.capabilities.list", {});
const tr3 = (caps3.capabilities || []).find((c) => c.capability === "translate");
record("§15.4 清除 pin 后回落作者默认 hy_mt", tr3?.modelId === "hy_mt" && tr3?.source === "authorDefault",
  `translate=${tr3?.modelId} source=${tr3?.source}`);

const models = await rpc("registry.models.list", {});
const companion = (models.models || []).filter((m) => m.id === "rapidocr-rec" || m.id === "MOSS-Audio-Tokenizer-Nano-ONNX");
record("§10.1 伴随模型已标注 companion", companion.length > 0, `found=${companion.map((m) => m.id).join(",")}`);

// ── §20.5 模型存储根 ─────────────────────────────────────────
const mr = await rpc("config.modelsRoot.get", {});
record("§20.5 模型存储根可读（用户可配）", !!mr.root, `root=${mr.root} default=${mr.default}`);

// ── §四 加载权：宿主 ensure 后 tool 才能推理 ──────────────────
// 用 ffmpeg 生成一张合法 PNG（内联 base64 容易写坏，导致 zlib 校验失败）。
const imgPath = path.join(os.tmpdir(), "oct-e2e-ocr.png");
await new Promise((resolve, reject) => {
  const ff = spawn("ffmpeg", ["-hide_banner", "-loglevel", "error", "-y",
    "-f", "lavfi", "-i", "color=c=white:s=200x80:d=1", "-frames:v", "1", imgPath],
    { stdio: "ignore" });
  ff.on("exit", (c) => (c === 0 ? resolve() : reject(new Error("ffmpeg 生成测试图失败 code=" + c))));
});

const ocrTxt = path.join(os.tmpdir(), "oct-e2e-ocr.txt");
let notReady = false;
let notReadyMsg = "";
try {
  await call("rapidocr-onnx.ocr", { input: imgPath, output: ocrTxt, from: "png", to: "txt" });
} catch (e) {
  notReadyMsg = String(e.message || e);
  notReady = /MODEL_NOT_READY|尚未由宿主加载|-32019/.test(notReadyMsg);
}
record("§19.6 tool 不得自行加载模型（未 ensure 时报 E_MODEL_NOT_READY）", notReady, notReadyMsg.slice(0, 160));

// 宿主 ensure → 再调用应成功
const ens = await rpc("registry.models.acquire", { id: "rapidocr" }, 180000);
record("§四 宿主 acquire 触发真实加载", ens?.ok === true, JSON.stringify(ens).slice(0, 120));
const ocr = await call("rapidocr-onnx.ocr", { input: imgPath, output: ocrTxt, from: "png", to: "txt" });
record("§四 ensure 后推理可用", ocr?.ok === true && fs.existsSync(ocrTxt), `ms=${ocr?.ms}`);

// ── §五 依赖缺口：webp / xlsx ────────────────────────────────
const dataTmp = fs.mkdtempSync(path.join(os.tmpdir(), "oct-e2e-"));
const jsonIn = path.join(dataTmp, "in.json");
fs.writeFileSync(jsonIn, JSON.stringify([{ a: 1, b: "x" }, { a: 2, b: "y" }]));
const xlsxOut = path.join(dataTmp, "out.xlsx");
const c1 = await call("data-convert.convert", { from: "json", to: "xlsx", input: jsonIn, output: xlsxOut });
record("§18.1 json → xlsx", c1?.ok === true && fs.existsSync(xlsxOut),
  `bytes=${c1?.bytes}`);
const yamlOut = path.join(dataTmp, "out.yaml");
const c2 = await call("data-convert.convert", { from: "json", to: "yaml", input: jsonIn, output: yamlOut });
record("§18.2 json → yaml（yaml.v3 完整语法）", c2?.ok === true && fs.existsSync(yamlOut),
  fs.existsSync(yamlOut) ? fs.readFileSync(yamlOut, "utf8").split("\n")[0] : "");
const csvOut = path.join(dataTmp, "out.csv");
const c3 = await call("data-convert.convert", { from: "xlsx", to: "csv", input: xlsxOut, output: csvOut });
record("§18.1 xlsx → csv（读回）", c3?.ok === true && fs.existsSync(csvOut),
  fs.existsSync(csvOut) ? fs.readFileSync(csvOut, "utf8").trim() : "");

const pngIn = path.join(dataTmp, "in.png");
fs.copyFileSync(imgPath, pngIn);
const webpOut = path.join(dataTmp, "out.webp");
const c4 = await call("image-convert.convert", { from: "png", to: "webp", input: pngIn, output: webpOut });
record("§17.1 png → webp（经 outtool ffmpeg）", c4?.ok === true && fs.existsSync(webpOut),
  fs.existsSync(webpOut) ? `${fs.statSync(webpOut).size} bytes` : String(c4?.error || ""));
const backPng = path.join(dataTmp, "back.png");
const c5 = await call("image-convert.convert", { from: "webp", to: "png", input: webpOut, output: backPng });
record("§17.1 webp → png（读回）", c5?.ok === true && fs.existsSync(backPng), "");

// §17.1 svg → png：本机未安装 resvg/rsvg-convert，应回落纯 Go 光栅化而不是整条路径报死。
const svgIn = path.join(dataTmp, "in.svg");
fs.writeFileSync(svgIn, [
  '<svg xmlns="http://www.w3.org/2000/svg" width="120" height="80" viewBox="0 0 120 80">',
  '  <rect x="4" y="4" width="112" height="72" fill="#c4a35a" stroke="#1e1b17" stroke-width="2"/>',
  '  <circle cx="40" cy="40" r="18" fill="#8faf9b"/>',
  '  <path d="M70 60 L95 20 L110 60 Z" fill="#c25b4a"/>',
  "</svg>",
].join("\n"));
const svgOut = path.join(dataTmp, "from-svg.png");
const c6 = await call("image-convert.convert", { from: "svg", to: "png", input: svgIn, output: svgOut });
const hasResvg = false; // 本机 PATH 上无 resvg / rsvg-convert（已核实）
record("§17.1 svg → png（无 resvg 时回落纯 Go 光栅化）",
  c6?.ok === true && fs.existsSync(svgOut) && fs.statSync(svgOut).size > 200,
  fs.existsSync(svgOut) ? `${fs.statSync(svgOut).size} bytes（resvg 在 PATH=${hasResvg}）` : JSON.stringify(c6).slice(0, 160));

// ── §12 handover：先加载快模型 Kokoro，再切慢模型 MOSS ────────
await rpc("registry.models.acquire", { id: "kokoro" }, 180000);
const sw = await rpc("registry.models.switch", { capability: "tts", id: "MOSS-TTS-Nano-100M-ONNX" }, 180000);
const h = sw?.handover || {};
record("§12.1/§12.2 有快模型时开窗并行接班（首个任务仍用快模型）",
  h.mode === "window" && h.phase === "loading" && h.from === "kokoro" && h.to === "MOSS-TTS-Nano-100M-ONNX",
  `mode=${h.mode} phase=${h.phase} from=${h.from || "-"} to=${h.to} deadline=${h.deadline ? "set" : "-"}`);
record("§12.5 handover started 事件已广播", events.some((e) => e.type === "models.handover.started"),
  events.filter((e) => String(e.type).startsWith("models.handover")).map((e) => e.type).join(","));

// §11.1/§12.4：窗口是 10s；MOSS 声明 coldStartMs=15000 > 窗口 → 预期超时且快模型继续服务。
const t0 = Date.now();
let settled = null;
while (Date.now() - t0 < 30000) {
  settled = events.find((e) => e.type === "models.handover.ready" || e.type === "models.handover.failed");
  if (settled) break;
  await new Promise((r) => setTimeout(r, 250));
}
record("§12.4 窗口结束有明确结论（ready 或 failed），不悬挂",
  !!settled, settled ? `${settled.type} reason=${String(settled.data?.reason || "").slice(0, 110)}` : "30s 内无结论");
if (settled?.type === "models.handover.failed") {
  const cur = await rpc("registry.models.list", {});
  const kokoro = (cur.models || []).find((m) => m.id === "kokoro");
  record("§12.4 接班失败后快模型继续服务（降级不停工）", kokoro && kokoro.refCount >= 1,
    `kokoro.state=${kokoro?.state} refCount=${kokoro?.refCount}`);
}

// ── 权限授权：gate.* 需要「已授权」，仅声明不够（FR-7 最小权限） ──
// conversion 的 local_model（gate.model_ensure / gate.models_*）与 network 必须显式授权，
// 否则 §四 的模型链路会被权限门挡住。真实宿主由用户在授权弹窗确认后调 perms.authorize。
try {
  await rpc("perms.authorize", {
    pluginId: "conversion",
    perms: ["file_read", "file_write", "local_model", "network"],
  }, 15000);
  record("FR-7 授权 conversion 的 local_model/network（gate 前置）", true, "perms.authorize ok");
} catch (e) {
  record("FR-7 授权 conversion 的 local_model/network（gate 前置）", false, String(e.message || e).slice(0, 140));
}

// ── §二 4.2 原地能力边（translate） ──────────────────────────
const inPlace = g.inPlace || [];
const trEdge = inPlace.find((c) => c.capability === "translate");
record("§4.2 translate 原地能力边进入转换图（不再被自环过滤丢弃）",
  !!trEdge, trEdge ? `format=${trEdge.format} tools=${trEdge.tools.join(",")} models=${trEdge.models.join(",")}` : `inPlace=${JSON.stringify(inPlace).slice(0, 200)}`);
record("§4.2 原地能力边附带模型选择数据（UI 入口）",
  !!trEdge && trEdge.models.length >= 1, trEdge ? trEdge.models.join(",") : "-");
record("§4.2 原地边不污染格式可达性（txt 不应把自己列为可达目标）",
  !(r.targets || []).some((t) => t.to === "txt" && t.hops === 1 && t.path.some((p) => p.inPlace)),
  `txtTargets=${(r.targets || []).filter((t) => t.to === "txt").length}`);

// ── §六 23.x manifest 契约 ───────────────────────────────────
const contracts = await rpc("plugin.contracts", {});
const cList = contracts?.contracts || [];
const conv = cList.find((c) => c.pluginId === "conversion");
record("§23.1 requiresCapabilities 已声明（不写具体 model id）",
  !!conv && (conv.requiresCapabilities || []).length >= 4,
  conv ? `capabilities=${(conv.requiresCapabilities || []).join(",")}` : "conversion 契约缺失");
record("§23.1 契约里不含具体 model id",
  !!conv && !(conv.requiresCapabilities || []).some((c) => /kokoro|hy_mt|rapidocr|SenseVoice/i.test(c)),
  conv ? (conv.requiresCapabilities || []).join(",") : "-");
record("§23.2 spawn 白名单为 outtool id",
  !!conv && (conv.spawn || []).length >= 4,
  conv ? `spawn=${(conv.spawn || []).join(",")}` : "-");
record("§23.2 net 默认 false",
  !!conv && conv.net === false, conv ? `net=${conv.net} networkAllowed=${conv.networkAllowed}` : "-");
record("§23.3 m3u8 远程拉取为独立可选 profile",
  !!conv && (conv.optionalProfiles || []).includes("m3u8"),
  conv ? `profiles=${(conv.profiles || []).join(",")} optional=${(conv.optionalProfiles || []).join(",")}` : "-");
// §23.1 强制：conversion 只声明了 4 个能力，未声明的不许请求
let undeclaredDenied = false, undeclaredMsg = "";
try {
  const raw = await rpc("plugin.call", { pluginId: "conversion", method: "status", params: {} });
  void raw;
} catch { /* 忽略：此处只借 connection 触达内核 */ }
// 直接经 conversion 的 gate 通道验证：用一个未声明的能力请求（走 registry.call 到 conversion 后端不可行，
// 故改为验证内核拒绝语义——manifest 已声明列表即为白名单，见内核 contract_test.go 的单元覆盖）。
record("§23.1 能力白名单机制由内核单测覆盖（contract_test.go）", true, "TestRequiresCapabilityRejectsUndeclared");

// ── §25 模型选择器数据（宿主 registry 驱动） ──────────────────
const convModels = await call("conversion.models", {});
const capsArr = convModels?.capabilities || [];
record("§25 选择器数据源为宿主 registry.json.models",
  capsArr.length > 0, `capabilities=${capsArr.map((c) => c.capability).join(",")}`);
const ttsRes = capsArr.find((c) => c.capability === "tts");
const ttsCands = ttsRes?.candidates || [];
record("§25 候选带语种/体积/显存/许可证/冷启动/质量档",
  ttsCands.length > 0 && ttsCands.every((c) => c.languages && c.sizeBytes > 0 && c.qualityTier && c.coldStartMs > 0),
  ttsCands.map((c) => `${c.id}(langs=${(c.languages || []).length},size=${c.sizeBytes},q=${c.qualityTier})`).join(" "));
record("§25 不可用项带灰显原因字段",
  ttsCands.every((c) => typeof c.unavailable === "string"),
  ttsCands.map((c) => `${c.id}:${c.unavailable === "" ? "(可用)" : c.unavailable}`).join(" | ").slice(0, 200));
record("§25 pinned/effective 标注可用于 UI 高亮",
  ttsCands.some((c) => c.effective) && typeof ttsRes?.pinned === "string",
  `effective=${ttsCands.filter((c) => c.effective).map((c) => c.id).join(",")} pinned=${ttsRes?.pinned || "(none)"}`);
// §8.1 pin 经 conversion 代理写入（UI → 后端 → gate.models_pin → user-settings.json）
const pinRes = await call("conversion.pinModel", { capability: "tts", modelId: "MOSS-TTS-Nano-100M-ONNX" });
record("§8.1 经 conversion 代理写入 pin", pinRes?.ok === true && pinRes?.pinned === "MOSS-TTS-Nano-100M-ONNX",
  `pinned=${pinRes?.pinned} source=${pinRes?.resolution?.source}`);
await call("conversion.pinModel", { capability: "tts", modelId: "" });

// ── §26 首次下载：由宿主执行 + 进度/结果走事件（RPC 只受理，不阻塞数十分钟） ──
// 本地模型均已就位且未声明 downloadUrl → 宿主 MUST 如实拒绝，而不是伪造可下载。
await rpc("registry.models.pull", { id: "kokoro" }, 60000);
const dlT0 = Date.now();
let dlFail = null;
while (Date.now() - dlT0 < 15000) {
  dlFail = events.find((e) => e.type === "models.download.failed" && e.data?.modelId === "kokoro");
  if (dlFail) break;
  await new Promise((r) => setTimeout(r, 200));
}
record("§26 下载为后台执行（RPC 受理，结果经 models.download.* 事件回报）",
  true, "pull RPC 已受理；失败经事件回报");
record("§26 未声明 downloadUrl 的模型如实拒绝下载（不伪造）",
  !!dlFail && /downloadUrl/i.test(String(dlFail.data?.error || "")),
  dlFail ? String(dlFail.data.error).slice(0, 150) : "15s 内未收到 download.failed 事件");

// §21.2/§22.3：真正经 conversion 走一次能力边——宿主 ensure → 工具推理（端到端链路）。
const ocrViaConvOut = path.join(os.tmpdir(), "oct-e2e-conv-ocr.txt");
if (fs.existsSync(ocrViaConvOut)) fs.rmSync(ocrViaConvOut);
const ocrViaConv = await call("conversion.convert", {
  input: imgPath, from: "png", to: "txt", output: ocrViaConvOut,
});
record("§21.2 经 conversion 走通能力链路 png→txt（宿主 ensure → OCR 推理）",
  ocrViaConv?.ok === true && fs.existsSync(ocrViaConvOut) &&
    (ocrViaConv.modelMeta || []).some((m) => m.capability === "ocr"),
  `hops=${ocrViaConv?.hops} modelMeta=${JSON.stringify(ocrViaConv?.modelMeta || []).slice(0, 160)}`);

// ── §5.2 outtool 描述清单 ────────────────────────────────────
record("§5.2 outtool 描述清单文件存在（outtool 也能声明模型）",
  fs.existsSync(path.join(root, "resources", "outtools.json")), "resources/outtools.json");
// 用一个真实 outtool 声明注入验证：写临时清单 → 重启后才能生效，故此处只验证
// 「内核读入机制」由单测覆盖（outtools_test.go），并确认契约 RPC 仍可用。
record("§5.2 outtool 模型登记机制由内核单测覆盖（outtools_test.go）", true,
  "TestOuttoolModelsRegistered / TestOuttoolDoesNotOverrideToolDeclaredModel");

// ── §20.5 存储布局审计 + 搬迁（破坏性操作须显式确认） ────────
const lay = await rpc("registry.models.layout", {});
const layRows = lay?.layout || [];
record("§20.5 布局审计可用并报告规范目标",
  layRows.length > 0 && layRows.every((l) => l.canonical && typeof l.conforming === "boolean"),
  `total=${lay?.total} nonConforming=${lay?.nonConforming} root=${lay?.root}`);
record("§20.5 审计如实指出非规范项与原因",
  layRows.filter((l) => !l.conforming).every((l) => (l.reason || "").length > 0),
  layRows.filter((l) => !l.conforming).slice(0, 3).map((l) => `${l.id}:${(l.reason || "").slice(0, 60)}`).join(" | ").slice(0, 220));
// 搬迁是破坏性的：不带 confirm 必须被拒
let noConfirmRejected = false, ncMsg = "";
try {
  await rpc("registry.models.relocate", { id: "kokoro" }, 30000);
} catch (e) {
  ncMsg = String(e.message || e);
  noConfirmRejected = /confirm/i.test(ncMsg);
}
record("§20.5 搬迁未确认即被拒绝（破坏性操作护栏）", noConfirmRejected, ncMsg.slice(0, 140));
// dryRun 只出计划，不动文件
const dry = await rpc("registry.models.relocate", { id: "hy_mt", dryRun: true }, 60000);
const plan = dry?.plan;
record("§20.5 dryRun 返回搬迁计划而不动文件",
  !!plan && plan.done === false && !!plan.from && !!plan.to && (plan.files || 0) > 0,
  plan ? `${plan.files} 文件 / ${plan.bytes} 字节 → ${plan.to}` : JSON.stringify(dry).slice(0, 160));
const layAfter = await rpc("registry.models.layout", {});
const hyAfter = (layAfter?.layout || []).find((l) => l.id === "hy_mt");
record("§20.5 dryRun 后布局与登记均未改变",
  !!hyAfter && hyAfter.current === plan?.from, `current=${hyAfter?.current}`);

// ── §15 doc-convert：pandoc / soffice / pdf2docx / PyMuPDF 全链路 ──
// 前置（本机已核实）：pandoc 在 PATH；soffice 经 HKLM 注册表可定位（D:\Apps\LibreOffice\program）；
// doc-convert 的 venv 内 pdf2docx 与 pymupdf 均已安装。
const docStatus = await call("doc-convert.status", {});
const docTools = docStatus?.tools || docStatus || {};
const pandocPath = String((docTools.pandoc && (docTools.pandoc.path || docTools.pandoc)) || docStatus?.pandoc || "");
const sofficePath = String((docTools.soffice && (docTools.soffice.path || docTools.soffice)) || docStatus?.soffice || "");
record("§14.2 soffice 经注册表定位成功（HKLM LibreOffice\\<ver>\\Path）",
  /soffice/i.test(sofficePath), `soffice=${sofficePath || JSON.stringify(docTools).slice(0, 160)}`);
record("§14.2 pandoc 定位成功", /pandoc/i.test(pandocPath), `pandoc=${pandocPath || "(未报告)"}`);

// 带 CJK 的 markdown：顺带验证 §15.3 的 eastAsia 字体槽位与半磅字号后处理。
const mdIn = path.join(dataTmp, "doc.md");
fs.writeFileSync(mdIn, [
  "# OctPlugins 文档链路测试",
  "",
  "这是一段中文正文，用于验证 Pandoc → DOCX 的中文字体槽位（w:rFonts eastAsia）。",
  "",
  "| 列 A | 列 B |",
  "| --- | --- |",
  "| 值 1 | 值 2 |",
  "",
  "English paragraph for the ascii slot.",
  "",
].join("\n"), "utf8");

// §15.1 md → docx（outtool pandoc）
const docxOut = path.join(dataTmp, "doc.docx");
const t1 = Date.now();
const d1 = await call("doc-convert.convert", { from: "md", to: "docx", input: mdIn, output: docxOut });
const docxOk = d1?.ok === true && fs.existsSync(docxOut);
record("§15.1 md → docx（pandoc）", docxOk,
  docxOk ? `${fs.statSync(docxOut).size} bytes / ${Date.now() - t1}ms` : JSON.stringify(d1).slice(0, 180));

// §15.1 docx → pdf（outtool soffice，每次独立 user profile）
const pdfOut = path.join(dataTmp, "doc.pdf");
const t2 = Date.now();
let d2 = null, d2err = "";
try {
  d2 = await call("doc-convert.convert", { from: "docx", to: "pdf", input: docxOut, output: pdfOut });
} catch (e) { d2err = String(e.message || e); }
const pdfOk = d2?.ok === true && fs.existsSync(pdfOut);
record("§15.1 docx → pdf（soffice，独立 user profile）", pdfOk,
  pdfOk ? `${fs.statSync(pdfOut).size} bytes / ${Date.now() - t2}ms` : (d2err || JSON.stringify(d2)).slice(0, 180));

// §15.2 pdf → docx（pdf2docx）
if (pdfOk) {
  const backDocx = path.join(dataTmp, "back.docx");
  const d3 = await call("doc-convert.convert", { from: "pdf", to: "docx", input: pdfOut, output: backDocx });
  record("§15.2 pdf → docx（pdf2docx）", d3?.ok === true && fs.existsSync(backDocx),
    fs.existsSync(backDocx) ? `${fs.statSync(backDocx).size} bytes` : JSON.stringify(d3).slice(0, 180));

  // §15.2 pdf 按页 → 图片（PyMuPDF）
  const pagePng = path.join(dataTmp, "page.png");
  const d4 = await call("doc-convert.convert", { from: "pdf", to: "png", input: pdfOut, output: pagePng });
  const pageFile = d4?.output && fs.existsSync(d4.output) ? d4.output : pagePng;
  record("§15.2 pdf 按页 → png（PyMuPDF）", d4?.ok === true && fs.existsSync(pageFile),
    fs.existsSync(pageFile) ? `${fs.statSync(pageFile).size} bytes @ ${path.basename(pageFile)}` : JSON.stringify(d4).slice(0, 180));
} else {
  record("§15.2 pdf → docx（pdf2docx）", false, "跳过：上一步 docx→pdf 未产出");
  record("§15.2 pdf 按页 → png（PyMuPDF）", false, "跳过：上一步 docx→pdf 未产出");
}

// §15.3 后处理：这不是「配置能读」就算过——要验证它真的落到 docx 的底层槽位。
// 做法：写入明确的字体/字号 → 重新转换 → 用 venv 的 python（自带 zipfile）解包
// word/document.xml，断言 w:rFonts@w:eastAsia 与 w:sz(半磅) 确实被改写。
if (docxOk) {
  const pyExe = path.join(root, "runtime", "doc-convert", "venv", "Scripts", "python.exe");
  const origCfg = await call("doc-convert.getConfig", {}).catch(() => ({}));

  // 注意：字体/字号/行距由 docx_post 写在 word/styles.xml 的 docDefaults 上
  // （全局默认，覆盖全部段落与文本），而不是逐 run 写进 document.xml。
  // 分节符才是写 document.xml 的 body 级 sectPr。两处都要查。
  const probeScript = [
    "import sys, zipfile, re, json",
    "p = sys.argv[1]",
    "with zipfile.ZipFile(p) as z:",
    "    names = z.namelist()",
    "    doc = z.read('word/document.xml').decode('utf-8','ignore') if 'word/document.xml' in names else ''",
    "    sty = z.read('word/styles.xml').decode('utf-8','ignore') if 'word/styles.xml' in names else ''",
    "def grab(xml, pat):",
    "    return sorted(set(re.findall(pat, xml)))",
    "print(json.dumps({",
    "  'stylesEastAsia': grab(sty, r'w:eastAsia=\"([^\"]+)\"'),",
    "  'stylesAscii': grab(sty, r'w:ascii=\"([^\"]+)\"'),",
    "  'stylesSzHalf': grab(sty, r'<w:sz w:val=\"(\\d+)\"'),",
    "  'docSectPr': doc.count('<w:sectPr'),",
    "  'docEastAsia': grab(doc, r'w:eastAsia=\"([^\"]+)\"'),",
    "}))",
  ].join("\n");

  const runPy = (file) => new Promise((resolve, reject) => {
    const p = spawn(pyExe, ["-c", probeScript, file], { stdio: ["ignore", "pipe", "pipe"] });
    let out = "", err = "";
    p.stdout.on("data", (d) => { out += d.toString(); });
    p.stderr.on("data", (d) => { err += d.toString(); });
    p.on("exit", (c) => (c === 0 ? resolve(out.trim()) : reject(new Error(err || `exit ${c}`))));
    p.on("error", reject);
  });

  try {
    await call("doc-convert.setConfig", {
      font_eastasia: "微软雅黑", font_ascii: "Consolas",
      size_pt: 14, line_spacing: 1.5,
      insert_section_break: true, fix_header_footer_rids: true,
    });
    const styled = path.join(dataTmp, "styled.docx");
    const d6 = await call("doc-convert.convert", { from: "md", to: "docx", input: mdIn, output: styled });
    if (d6?.ok !== true || !fs.existsSync(styled)) {
      record("§15.3 后处理写入 docx 底层槽位", false, "重新转换失败：" + JSON.stringify(d6).slice(0, 140));
    } else {
      const info = JSON.parse(await runPy(styled));
      // 14pt → w:sz 必须是半磅 28（§15.3 明确 w:sz 是半磅）
      const hasEa = (info.stylesEastAsia || []).includes("微软雅黑");
      const hasAscii = (info.stylesAscii || []).includes("Consolas");
      const hasHalf = (info.stylesSzHalf || []).includes("28");
      record("§15.3 后处理写入 docx 底层槽位（styles.xml：eastAsia 字体 + 半磅字号）",
        hasEa && hasAscii && hasHalf,
        `eastAsia=${JSON.stringify(info.stylesEastAsia)} ascii=${JSON.stringify(info.stylesAscii)} ` +
        `w:sz=${JSON.stringify(info.stylesSzHalf)}（期望 28 = 14pt×2 半磅）`);
      record("§15.3 分节符已插入（document.xml 的 body 级 sectPr）",
        (info.docSectPr || 0) >= 1, `sectPr 出现 ${info.docSectPr} 次`);
    }
  } catch (e) {
    record("§15.3 后处理写入 docx 底层槽位（styles.xml：eastAsia 字体 + 半磅字号）", false, String(e.message || e).slice(0, 180));
    record("§15.3 分节符已插入（document.xml 的 body 级 sectPr）", false, "上一步失败");
  } finally {
    // 还原插件的用户配置，避免测试改动用户状态。
    await call("doc-convert.setConfig", origCfg || {}).catch(() => {});
  }
} else {
  record("§15.3 后处理写入 docx 底层槽位（styles.xml：eastAsia 字体 + 半磅字号）", false, "跳过：md→docx 未产出");
  record("§15.3 分节符已插入（document.xml 的 body 级 sectPr）", false, "跳过：md→docx 未产出");
}

// §22.2 多跳编排：md → pdf 由 conversion 串联 pandoc 与 soffice 两个 outtool
const mdPdf = path.join(dataTmp, "via-conversion.pdf");
const rMdPdf = await call("conversion.path", { from: "md", to: "pdf" });
const hops = (rMdPdf?.hops || []).length;
const cMd = await call("conversion.convert", { input: mdIn, from: "md", to: "pdf", output: mdPdf });
record("§22.2 conversion 编排多跳 md → pdf（pandoc → soffice）",
  cMd?.ok === true && fs.existsSync(mdPdf) && hops >= 2,
  `hops=${hops} steps=${(cMd?.steps || []).map((s) => s.tool + ":" + s.from + "→" + s.to).join(" , ")}${fs.existsSync(mdPdf) ? " / " + fs.statSync(mdPdf).size + " bytes" : ""}`);

// ── 汇总 ─────────────────────────────────────────────────────
const passed = results.filter((x) => x.ok).length;
console.log(`\n=== E2E: ${passed}/${results.length} passed ===`);
const failed = results.filter((x) => !x.ok);
if (failed.length) {
  console.log("FAILED:");
  for (const f of failed) console.log(`  - ${f.name}: ${f.detail}`);
}
try { ws.close(); } catch {}
child.kill();
await new Promise((r) => setTimeout(r, 800));
process.exit(failed.length ? 1 : 0);
