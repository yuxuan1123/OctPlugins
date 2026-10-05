// UI 接线一致性检查（conversion 插件页）
//
// 目的：捕获「JS 引用了 HTML 里不存在的元素 id」这类最容易犯、又只在运行时才暴露的错。
// 不需要浏览器：只做静态交叉比对，因此可以放进每次改 UI 后的快速自检。
//
// 用法: node tools/check-ui-wiring.mjs [插件目录…]（缺省检查 conversion）
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const targets = process.argv.slice(2);
const plugins = targets.length ? targets : ["plugins/conversion"];

let failures = 0;
for (const rel of plugins) {
  const dir = path.join(root, rel, "ui");
  const htmlPath = path.join(dir, "index.html");
  const jsPath = path.join(dir, "src", "app.js");
  if (!fs.existsSync(htmlPath) || !fs.existsSync(jsPath)) {
    console.log(`SKIP ${rel}（缺 index.html 或 src/app.js）`);
    continue;
  }
  const html = fs.readFileSync(htmlPath, "utf8");
  const js = fs.readFileSync(jsPath, "utf8");

  const defined = new Set([...html.matchAll(/id="([^"]+)"/g)].map((m) => m[1]));
  const referenced = new Set([...js.matchAll(/\$\("([^"]+)"\)/g)].map((m) => m[1]));
  // getElementById 也应纳入比对
  for (const m of js.matchAll(/getElementById\("([^"]+)"\)/g)) referenced.add(m[1]);

  const missing = [...referenced].filter((id) => !defined.has(id)).sort();
  if (missing.length) {
    failures++;
    console.log(`FAIL ${rel}：JS 引用了 HTML 中不存在的 id`);
    for (const id of missing) console.log(`   - ${id}`);
  } else {
    console.log(`PASS ${rel}：${referenced.size} 个被引用的 id 全部存在于 index.html`);
  }

  // 反向提示：HTML 定义了但 JS 从不引用（多为纯展示元素，仅作信息）
  const unreferenced = [...defined].filter((id) => !referenced.has(id) && !id.startsWith("dlg")).sort();
  if (unreferenced.length) {
    console.log(`   info: HTML 定义但 JS 未直接引用的 id（可能纯展示或经 querySelector 访问）: ${unreferenced.join(", ")}`);
  }
}

process.exit(failures ? 1 : 0);
