// OctPlugins — conversion 全量「直接边」测试（只测 1 跳，不做多跳编排）
// 用法: node tools/e2e-conv-all.mjs
// 步骤: 重建/拉起 kerneld → WS → kernel.hello → 从 conversion.graph 枚举全部 1 跳边
//       → 用 ffmpeg/pandoc/soffice + 各 tool 自身引导种子输入 → 逐边 conversion.convert 断言
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";

const require = createRequire(import.meta.url);
const WebSocket = require("../ui/node_modules/ws");

const root = process.cwd();
const work = fs.mkdtempSync(path.join(os.tmpdir(), "oct-conv-all-"));
const SOFFICE = "D:\\Apps\\LibreOffice\\program\\soffice.exe";
const PANDOC = "D:\\LanguageTool\\Anaconda\\Library\\bin\\pandoc.exe";
const FFMPEG = "D:\\LanguageTool\\ffmpeg\\bin\\ffmpeg.exe";
const results = [];

// 把外部引擎目录加到 PATH，好让 daemon 里的各 tool 能定位 pandoc/soffice/ffmpeg
const audPath = [
  path.dirname(SOFFICE), path.dirname(PANDOC), path.dirname(FFMPEG),
  process.env.PATH || "",
].join(";");

function record(name, ok, detail) {
  results.push({ name, ok, detail });
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${detail ? "  — " + detail : ""}`);
}

// ── 工具链：ffmpeg / soffice / pandoc 直接生成种子文件 ──
function runTool(cmd, args, timeoutMs = 60000) {
  const r = spawnSync(cmd, args, { encoding: "utf8", timeout: timeoutMs });
  return { ok: r.status === 0, out: (r.stdout || "").trim(), err: (r.stderr || "").trim() };
}
function ff(args, timeout) { return runTool(FFMPEG, ["-hide_banner", "-y", "-loglevel", "error", ...args], timeout); }
function soffice(args, timeout) { return runTool(SOFFICE, ["-env:UserInstallation=file:///" + work.replace(/\\/g, "/") + "/lo-profile", "--headless", ...args], timeout); }
function pandoc(args, timeout) { return runTool(PANDOC, args, timeout); }

const fmtIn = {}; // format(canonical) → 输入文件绝对路径

// 反查 nrm 别名归一（与 conversion 的 norm/fmtAlias 对齐）
const fmtAlias = { jpeg: "jpg", ppt: "pptx", yml: "yaml", htm: "html", tif: "tiff" };
function norm(f) {
  let s = String(f).toLowerCase().trim().replace(/^\./, "");
  return fmtAlias[s] || s;
}

// ── 种子生成 ──
function addInput(fmt, p) { if (p && fs.existsSync(p)) fmtIn[norm(fmt)] = p; }

// 文本类
const txt = path.join(work, "seed.txt");
fs.writeFileSync(txt, "Hello OctPlugins. 中英混合测试文本。line two.\n", "utf8"); addInput("txt", txt);
const md = path.join(work, "seed.md");
fs.writeFileSync(md, "# Title 测试\n\nParagraph 一段。| A | B |\n|---|---|\n| 1 | 2 |\n", "utf8"); addInput("md", md);
const jsonP = path.join(work, "seed.json");
fs.writeFileSync(jsonP, JSON.stringify([{ a: 1, b: "x" }, { a: 2, b: "y" }])); addInput("json", jsonP);
const csvP = path.join(work, "seed.csv");
fs.writeFileSync(csvP, "a,b\n1,x\n2,y\n", "utf8"); addInput("csv", csvP);
const yamlP = path.join(work, "seed.yaml");
fs.writeFileSync(yamlP, "- a: 1\n  b: x\n- a: 2\n  b: y\n", "utf8"); addInput("yaml", yamlP);
const htmlP = path.join(work, "seed.html");
fs.writeFileSync(htmlP, "<html><body><h1>测试</h1><p>hello</p></body></html>\n", "utf8"); addInput("html", htmlP);
const svgP = path.join(work, "seed.svg");
fs.writeFileSync(svgP, '<svg xmlns="http://www.w3.org/2000/svg" width="120" height="80" viewBox="0 0 120 80"><rect x="4" y="4" width="112" height="72" fill="#c4a35a"/><circle cx="40" cy="40" r="18" fill="#8faf9b"/></svg>\n', "utf8"); addInput("svg", svgP);

// 图像种子
const pngIn = path.join(work, "seed.png");
ff(["-f", "lavfi", "-i", "color=c=white:s=160x120:d=1", "-frames:v", "1", pngIn]); addInput("png", pngIn);

// 音频种子 (16k mono，适配 stt)
const wavIn = path.join(work, "seed.wav");
ff(["-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-ar", "16000", "-ac", "1", wavIn]); addInput("wav", wavIn);

// 视频种子
const mp4In = path.join(work, "seed.mp4");
ff(["-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", mp4In]); addInput("mp4", mp4In);

// 文档种子（docx 用 pandoc；doc 用无 UserInstallation 的 soffice——本机带 profile 导不出 .doc）
pandoc([md, "-o", path.join(work, "seed.docx")]);
addInput("docx", path.join(work, "seed.docx"));
runTool(SOFFICE, ["--headless", "--norestore", "--nolockcheck", "--convert-to", "doc:MS Word 97", "--outdir", work, path.join(work, "seed.docx")]);
addInput("doc", path.join(work, "seed.doc"));
pandoc([md, "-t", "pptx", "-o", path.join(work, "seed.pptx")]);
addInput("pptx", path.join(work, "seed.pptx"));
// pdf 种子由内核起来后经 doc-convert txt→pdf 生成（已知好 pdf，见下）

// 表数据派生（等内核起来后通过 tool 生成）
// 收集依赖内核早期阶段的派生在后面执行
console.log("[seeds] 已生成种子: " + Object.keys(fmtIn).sort().join(","));

// ── 拉起内核 ──
const registryPath = path.join(root, "state", "registry.json");
if (fs.existsSync(registryPath)) fs.rmSync(registryPath);

const child = spawn(path.join(root, "kernel", "kerneld.exe"), [], { cwd: root, stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, PATH: audPath } });
let stderrBuf = "";
child.stderr.on("data", (d) => { stderrBuf += d.toString(); });

const auth = await new Promise((resolve, reject) => {
  let buf = "";
  const to = setTimeout(() => reject(new Error("等待内核 auth 超时")), 90000);
  child.stdout.on("data", (d) => {
    buf += d.toString();
    const line = buf.split(/\r?\n/)[0];
    if (line && line.trim().startsWith("{")) {
      try { const { auth } = JSON.parse(line); clearTimeout(to); resolve(auth); } catch { /* wait */ }
    }
  });
  child.on("exit", (c) => { clearTimeout(to); reject(new Error(`内核提前退出 code=${c}\n${stderrBuf}`)); });
});

const ws = new WebSocket(`ws://127.0.0.1:${auth.port}`);
let nextId = 1;
const pending = new Map();
ws.on("message", (raw) => {
  const m = JSON.parse(raw.toString());
  if (m.id && pending.has(m.id)) {
    const { resolve, reject } = pending.get(m.id);
    pending.delete(m.id);
    if (m.error) reject(new Error(`${m.error.code}: ${m.error.data ? JSON.stringify(m.error.data) : m.error.message}`));
    else resolve(m.result);
  }
});
function rpc(method, params, timeoutMs = 180000) {
  const id = nextId++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ v: 1, jsonrpc: "2.0", id, method, params }));
    setTimeout(() => { if (pending.has(id)) { pending.delete(id); reject(new Error(`${method} timeout`)); } }, timeoutMs);
  });
}
await new Promise((res, rej) => { ws.on("open", res); ws.on("error", rej); });
await rpc("kernel.hello", { client: "octplugin-host", token: auth.token });
const call = async (fn, params) => {
  const raw = await rpc("registry.call", { name: fn, params, timeoutMs: 180000 }, 180000);
  if (raw && typeof raw === "object" && "ok" in raw && "result" in raw) return raw.result;
  return raw;
};

// 授权 conversion（gate 前置）
try { await rpc("perms.authorize", { pluginId: "conversion", perms: ["file_read", "file_write", "local_model", "network"] }, 15000); } catch {}

// ── 用 data-convert/image-convert/media-convert 派生剩余种子 ──
// 表文件
const dc = async (from, to, out) => (await call("data-convert.convert", { from, to, input: fmtIn[from], output: out })).ok;
try {
  const xlsxP = path.join(work, "derv.xlsx");
  if (await dc("json", "xlsx", xlsxP)) addInput("xlsx", xlsxP);
  const xlsP = path.join(work, "derv.xls");
  const sx = soffice(["--convert-to", "xls", "--outdir", work, path.join(work, "derv.xlsx")]);
  if (sx.ok && fs.existsSync(path.join(work, "derv.xls"))) addInput("xls", path.join(work, "derv.xls"));
} catch (e) { console.log(" [seed data] xlsx/xls:", String(e.message).slice(0, 120)); }

// 图像派生
const ic = async (from, to, out) => (await call("image-convert.convert", { from, to, input: fmtIn[from], output: out })).ok;
try {
  if (await ic("png", "jpg", path.join(work, "derv.jpg"))) addInput("jpg", path.join(work, "derv.jpg"));
  if (await ic("png", "bmp", path.join(work, "derv.bmp"))) addInput("bmp", path.join(work, "derv.bmp"));
  if (await ic("png", "gif", path.join(work, "derv.gif"))) addInput("gif", path.join(work, "derv.gif"));
  if (await ic("png", "webp", path.join(work, "derv.webp"))) addInput("webp", path.join(work, "derv.webp"));
  if (await ic("png", "avif", path.join(work, "derv.avif"))) addInput("avif", path.join(work, "derv.avif"));
} catch (e) { console.log(" [seed image]", String(e.message).slice(0, 120)); }

// 音视频派生
const mc = async (from, to, out) => (await call("media-convert.convert", { from, to, input: fmtIn[from], output: out })).ok;
try {
  for (const t of ["mp3", "aac", "opus", "flac"]) if (await mc("wav", t, path.join(work, "derv." + t))) addInput(t, path.join(work, "derv." + t));
  if (await mc("wav", "alac", path.join(work, "derv-m4a.m4a"))) addInput("alac", path.join(work, "derv-m4a.m4a"));
  for (const t of ["mkv", "mov"]) if (await mc("mp4", t, path.join(work, "derv." + t))) addInput(t, path.join(work, "derv." + t));
  // m3u8 输出为 HLS 目录/播放列表
  const m3u8Out = path.join(work, "derv.m3u8");
  const m3u8R = await call("media-convert.convert", { from: "mp4", to: "m3u8", input: fmtIn["mp4"], output: m3u8Out });
  if (m3u8R && fs.existsSync(m3u8Out)) addInput("m3u8", m3u8Out);
} catch (e) { console.log(" [seed media]", String(e.message).slice(0, 120)); }

// 文档派生（doc-convert）
const dc2 = async (from, to, out) => (await call("doc-convert.convert", { from, to, input: fmtIn[from], output: out })).ok;
// pdf 种子：用 doc-convert txt→pdf（好 pdf），避免 soffice html→pdf 的坏文件导致 pdf→* 误判
if (!fmtIn["pdf"]) {
  try { if (await dc2("txt", "pdf", path.join(work, "derv.pdf"))) addInput("pdf", path.join(work, "derv.pdf")); }
  catch (e) { console.log(" [seed pdf]", String(e.message).slice(0, 120)); }
}

// ── 枚举全部 1 跳直接边 ──
const g = await call("conversion.graph", {});
const edges = g.edges || [];
const plain = new Map(); // "from\x00to" → tool 列表
const caps = new Map();  // "from\x00to\x00capability" → edge
for (const e of edges) {
  const key = `${norm(e.from)}\x00${norm(e.to)}`;
  if (e.inPlace || e.requiresCapability) { caps.set(`${key}\x00${e.requiresCapability}`, e); continue; }
  const t = plain.get(key) || { edge: [] };
  t.edge.push(e);
  if (!plain.has(key)) plain.set(key, t);
}

record("图枚举：直接(非能力/非原地)边", plain.size > 0, `plain=${plain.size} capEdges=${caps.size} totalEdges=${edges.length}`);

// ── 逐条跑直接边 ──
// progress 事件显示每跳 tool
let convCount = 0, failCount = 0;
const failList = [];
async function runDirect(key, label) {
  const [from, to] = key.split("\x00");
  const inFile = fmtIn[from];
  if (!inFile) {
    convCount++;
    record(`${label} ${from}→${to}`, false, "SKIP: 无可用输入种子 (env)");
    failList.push(`${label} ${from}→${to} SKIP:无种子`);
    return;
  }
  const outBase = path.join(work, `out_${convCount}_${from}_to_${to}`);
  const outP = `${outBase}.${to}`;
  convCount++;
  const t0 = Date.now();
  try {
    const res = await call("conversion.convert", { input: inFile, from, to, output: outP });
    const realOut = (res && typeof res.output === "string" && res.output) ? res.output : outP;
    const ok = res && res.ok === true && fs.existsSync(realOut) && fs.statSync(realOut).size > 0;
    const step = (res.steps || []).map((s) => `${s.from}→${s.to}@${s.tool}`).join(",");
    record(`${label} ${from}→${to}`, ok, ok ? `${(Date.now() - t0)}ms ${step ? step : ""}` : JSON.stringify(res).slice(0, 200));
    if (!ok) failList.push(`${label} ${from}→${to}`);
  } catch (e) {
    failCount++;
    record(`${label} ${from}→${to}`, false, "ERR " + String(e.message).slice(0, 160));
    failList.push(`${label} ${from}→${to} ERR ${String(e.message).slice(0, 80)}`);
  }
}

// 普通边
for (const [key, t] of plain) runDirect(key, "普通");

// 能力边：先加载模型再跑
async function acquireModels() {
  const modelOf = { stt: "SenseVoiceSmall", tts: "kokoro", ocr: "rapidocr", translate: "hy_mt" };
  for (const cap of ["ocr", "stt", "tts", "translate"]) {
    const id = modelOf[cap];
    try { await rpc("registry.models.acquire", { id }, 240000); record(`模型加载 ${cap}(${id})`, true, ""); }
    catch (e) { record(`模型加载 ${cap}(${id})`, false, String(e.message).slice(0, 140)); }
  }
}
await acquireModels();
for (const [key, e] of caps) {
  const cap = e.requiresCapability;
  if (e.inPlace) {
    // 原地能力：txt→txt translate
    convCount++;
    const inFile = fmtIn["txt"];
    const outP = path.join(work, `out_${convCount}_translate.txt`);
    try {
      const res = await call("conversion.convert", { input: inFile, from: "txt", to: "txt", capability: "translate", output: outP });
      const ok = res && res.ok === true && fs.existsSync(outP) && fs.statSync(outP).size > 0;
      record(`能力 原地 translate txt→txt`, ok, ok ? `provider=${res.provider} model=${res.modelId}` : JSON.stringify(res).slice(0, 200));
      if (!ok) failList.push("能力 原地 translate txt→txt");
    } catch (err) { failCount++; record(`能力 原地 translate txt→txt`, false, "ERR " + String(err.message).slice(0, 160)); failList.push("原地 translate ERR"); continue; }
  } else {
    const [from, to] = key.split("\x00");
    await runDirect(key, `能力(${cap})`);
  }
}

// 能力边 from==to 的多个 cap 处理：translate 之外(stt/tts/ocr 的 from≠to)已覆盖；此处去重避免重复
// （caps 中可能有 from==to 的非 translate 能力边，已由 runDirect 处理 or 跳过）

const passed = results.filter((x) => x.ok).length;
console.log(`\n=== 直接边 E2E: ${passed}/${results.length} passed ===`);
console.log(`failed 边数=${failList.length}`);
if (failList.length) { console.log("FAILED:"); for (const f of failList) console.log(`  - ${f}`); }
try { ws.close(); } catch {}
child.kill();
await new Promise((r) => setTimeout(r, 800));
process.exit(failCount === 0 && failList.length === 0 ? 0 : 1);