// 诊断 pdf→* (doc-convert) 与 bmp→* (ocr) 直调底层 tool，暴露 conversion 包装隐藏的真实错误
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
const require = createRequire(import.meta.url);
const WebSocket = require("../ui/node_modules/ws");
const root = process.cwd();
const work = fs.mkdtempSync(path.join(os.tmpdir(), "oct-diag-"));
const PANDOC = "D:\\LanguageTool\\Anaconda\\Library\\bin\\pandoc.exe";
const FFMPEG = "D:\\LanguageTool\\ffmpeg\\bin\\ffmpeg.exe";
const audPath = "D:\\Apps\\LibreOffice\\program" + path.delimiter + path.dirname(PANDOC) + path.delimiter + path.dirname(FFMPEG) + path.delimiter + (process.env.PATH || "");

function runTool(cmd, args, t=30000){ const r=spawnSync(cmd,args,{encoding:"utf8",timeout:t}); return {ok:r.status===0,out:(r.stdout||"").trim(),err:(r.stderr||"").trim()}; }
function ff(args){ return runTool(FFMPEG,["-hide_banner","-y","-loglevel","error",...args]); }
ff(["-f","lavfi","-i","color=c=white:s=160x120:d=1","-frames:v","1",path.join(work,"s.png")]);

const registryPath = path.join(root,"state","registry.json");
if (fs.existsSync(registryPath)) fs.rmSync(registryPath);
const child = spawn(path.join(root,"kernel","kerneld.exe"),[],{cwd:root,stdio:["ignore","pipe","pipe"],env:{...process.env,PATH:audPath}});
let sb=""; child.stderr.on("data",d=>sb+=d.toString());
const auth = await new Promise((res,rej)=>{let buf="";const to=setTimeout(()=>rej(new Error("boot timeout")),90000);child.stdout.on("data",d=>{buf+=d.toString();const l=buf.split(/\r?\n/)[0];if(l&&l.trim().startsWith("{")){try{const{auth}=JSON.parse(l);clearTimeout(to);res(auth);}catch{}}});child.on("exit",c=>{clearTimeout(to);rej(new Error("exit "+c+"\n"+sb));});});
const ws = new WebSocket(`ws://127.0.0.1:${auth.port}`);
let id=1; const pend=new Map();
ws.on("message",raw=>{const m=JSON.parse(raw.toString());if(m.id&&pend.has(m.id)){const{res,rej}=pend.get(m.id);pend.delete(m.id);m.error?rej(new Error(m.error.code+": "+(m.error.data?JSON.stringify(m.error.data):m.error.message))):res(m.result);}});
function rpc(method,params,to=180000){const i=id++;return new Promise((res,rej)=>{pend.set(i,{res,rej});ws.send(JSON.stringify({v:1,jsonrpc:"2.0",id:i,method,params}));setTimeout(()=>{if(pend.has(i)){pend.delete(i);rej(new Error(method+" timeout"));}},to);});}
await new Promise((r,j)=>{ws.on("open",r);ws.on("error",j);});
await rpc("kernel.hello",{client:"diag",token:auth.token});
const call=async(fn,params)=>{const raw=await rpc("registry.call",{name:fn,params,timeoutMs:180000},180000);if(raw&&typeof raw==="object"&&"ok" in raw&&"result" in raw)return raw.result;return raw;};
try{await rpc("perms.authorize",{pluginId:"conversion",perms:["file_read","file_write","local_model","network"]},15000);}catch{}

// pdf 输入：先 txt→pdf (doc-convert 已知可行)
const mdP=path.join(work,"in.md"); fs.writeFileSync(mdP,"# T\nhello 测试正文用于 pdf。\n## S2\n一行。\n","utf8");
const pdfIn=path.join(work,"in.pdf");
const pre=await call("doc-convert.convert",{from:"txt",to:"pdf",input:mdP,output:pdfIn});
console.log("pre txt→pdf:",JSON.stringify(pre).slice(0,160), "exists="+fs.existsSync(pdfIn)+" size="+(fs.existsSync(pdfIn)?fs.statSync(pdfIn).size:"-"));

// bmp 输入
const bmpP=path.join(work,"s.bmp");
await call("image-convert.convert",{from:"png",to:"bmp",input:path.join(work,"s.png"),output:bmpP});
console.log("bmp exists="+fs.existsSync(bmpP));

console.log("\n--- doc-convert.status ---");
try{ console.log(JSON.stringify(await call("doc-convert.status",{})).slice(0,400)); }catch(e){ console.log("status ERR",String(e.message).slice(0,300)); }

for (const t of ["png","jpg","docx","doc"]) {
    const out=path.join(work,`pdf-to-${t}.${t}`);
    try{
      const r=await call("doc-convert.convert",{from:"pdf",to:t,input:pdfIn,output:out});
      const realOut=(r?.output)||out;
      console.log(`pdf→${t}: ok=${r&&r.ok} exists=${fs.existsSync(realOut)} size=${fs.existsSync(realOut)?fs.statSync(realOut).size:"-"} outputs=${JSON.stringify(r&&r.outputs)} err=${r&&r.error?JSON.stringify(r.error):"-"}`);
    }catch(e){ console.log(`pdf→${t}: ERR ${String(e.message).slice(0,260)}`); }
  }
// ocr bmp 直调（先加载模型）
await rpc("registry.models.acquire",{id:"rapidocr"},240000);
// ffmpeg 也生成一个 bmp 作对照
const fbmp=path.join(work,"ff.bmp");
runTool(FFMPEG,["-hide_banner","-y","-loglevel","error","-f","lavfi","-i","color=c=white:s=160x120:d=1","-frames:v","1",fbmp]);
console.log("ff bmp exists="+fs.existsSync(fbmp)+" size="+(fs.existsSync(fbmp)?fs.statSync(fbmp).size:"-"));
for (const [tag,fp] of [["image-convert",bmpP],["ffmpeg",fbmp]]) {
  try{
    const r=await call("rapidocr-onnx.ocr",{input:fp,from:"bmp",to:"txt",output:path.join(work,"s-ocr-"+tag+".txt")});
    console.log(`rapidocr bmp(${tag}): ok=${r&&r.ok} err=${r&&r.error?JSON.stringify(r.error):"-"}`);
  }catch(e){ console.log(`rapidocr bmp(${tag}): ERR ${String(e.message).slice(0,200)}`); }
}

try{ws.close();}catch{} child.kill(); await new Promise(r=>setTimeout(r,600));