/**
 * oct_sdk —— OCTplugins Node 插件 SDK（§16.5 / §11.2–§11.4 / §10.5）。
 *
 * 契约（与 Python SDK 等价）：
 *   - fd 1 为协议专用流（NDJSON），语言层默认输出（console.log）改道 stderr（§11.2）；
 *   - 启动后立即发 $/handshake（token 取自 env OCT_CHANNEL_TOKEN）（§11.4）；
 *   - 异步读循环保证 ping 独立应答，不阻塞业务逻辑（§13.3）；
 *   - busy 经上下文风格 API 申请/释放（§10.5）。
 */
'use strict';

const readline = require('readline');
const { EventEmitter } = require('events');

// §19 错误码/诊断码派生自 pkg/protocol（勿硬编码）。
const { CODES, DIAG_BY_CODE } = require('./protocol_errors');
const ERR_INTERNAL = -32603; // JSON-RPC 2.0 通用内部错误（非 §19 插件域私有表）

const PROTOCOL_VERSION = 1;

// ── fd1 接管（§11.2）：fd 1 为协议专用流。node 的 console.* 默认写 fd1，
// 仅改 console.log/error 会漏掉 info/debug/dir/table/trace 等仍写 fd1 → 污染协议。
// 故把所有 console.* 输出路径改道 stderr，fd 1 只留给 write() 的 NDJSON。
for (const k of ['log', 'info', 'debug', 'warn', 'error', 'dir', 'table', 'trace']) {
  console[k] = (...a) => process.stderr.write(a.map(String).join(' ') + '\n');
}

const protoOut = process.stdout; // fd1：协议专用流（NDJSON）

let nextId = 0;
const pending = new Map();
const handlers = new Map();

function write(msg) {
  protoOut.write(JSON.stringify(msg) + '\n');
}

function post(method, params, timeoutMs = 30000) {
  return new Promise((resolve, reject) => {
    const id = ++nextId;
    pending.set(id, { resolve, reject, timer: setTimeout(() => {
      pending.delete(id);
      reject(new Error(`call ${method} timeout`));
    }, timeoutMs) });
    write({ jsonrpc: '2.0', id, method, params: params ?? {} });
  });
}

// ── 路径访问器（§5.2） ─────────────────────────────────────────────
function homeDir() {
  return process.env.OCT_PLUGIN_HOME || process.cwd();
}
function dataDir() {
  return process.env.OCT_PLUGIN_DATA || require('path').join(homeDir(), 'data');
}
function cacheDir() {
  const d = require('path').join(dataDir(), 'cache');
  require('fs').mkdirSync(d, { recursive: true });
  return d;
}

// ── 通道与 gate（§11.6 / §16.5） ──────────────────────────────────
const channel = {
  call: (name, params) => post('registry.call', { name, params }),
  emit: (type, data) => post('event.emit', { type, data }),
  busy: (maxMs) => post('sdk.busy', { maxBusyMs: maxMs }),
};

const gate = {
  readFile: (path) => post('gate.file_read', { perm: 'file_read', path }),
  fileExists: (path) => post('gate.file_exists', { path }),
  writeFile: (path, data, opts = {}) => post('gate.file_write', { path, data }),
  exec: (command, args = [], opts = {}) => post('gate.execute_command', { command, args, timeoutMs: opts.timeoutMs || 15000 }),
};

/** §10.5 busy 上下文（async 友好）：busy(maxMs, fn)。 */
async function busy(maxMs, fn) {
  await post('sdk.busy', { maxBusyMs: maxMs });
  try {
    return await fn();
  } finally {
    await post('sdk.busy', { maxBusyMs: 0 });
  }
}

// ── 主循环（独立事件循环，ping 独立应答 §13.3） ──────────────────
const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });

rl.on('line', (line) => {
  line = line.trim();
  if (!line) return;
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    return;
  }
  if (msg && typeof msg.method === 'string') {
    const id = msg.id;
    const params = msg.params || {};
    const method = msg.method;
    if (method === 'ping') {
      write({ jsonrpc: '2.0', id, result: { pong: true } });
    } else if (method === 'shutdown') {
      write({ jsonrpc: '2.0', id, result: { ok: true } });
      process.exit(0);
    } else {
      const fn = handlers.get(method);
      if (!fn) {
        write({ jsonrpc: '2.0', id, error: { code: CODES.E_METHOD_NOT_FOUND, message: `method not found: ${method}`, data: { diag: 'E_METHOD_NOT_FOUND' } } });
        return;
      }
      Promise.resolve(fn(params))
        .then((result) => write({ jsonrpc: '2.0', id, result }))
        .catch((e) => write({ jsonrpc: '2.0', id, error: { code: ERR_INTERNAL, message: String(e), data: { diag: 'E_INTERNAL' } } }));
    }
    return;
  }
  if (msg && typeof msg.id === 'number' && pending.has(msg.id)) {
    const p = pending.get(msg.id);
    pending.delete(msg.id);
    clearTimeout(p.timer);
    if (msg.error) p.reject(new Error(JSON.stringify(msg.error)));
    else p.resolve(msg.result);
  }
});

// ── 入口 ──────────────────────────────────────────────────────────
function register(method, fn) {
  handlers.set(method, fn);
  return fn;
}

/**
 * §16.5 sdk.run(handler)：
 *   1) 首帧 $/handshake（token 经 env 注入，§11.4）；
 *   2) 注册 handler.methods；执行 handler.main(channel, args)。
 */
function run(handler, args) {
  const token = process.env.OCT_CHANNEL_TOKEN || '';
  write({ jsonrpc: '2.0', method: '$/handshake', params: { protocolVersion: PROTOCOL_VERSION, token } });
  if (handler && typeof handler === 'object') {
    for (const [k, v] of Object.entries(handler.methods || {})) handlers.set(k, v);
  }
  if (handler && typeof handler.main === 'function') {
    handler.main(channel, args);
  }
  return handler;
}

module.exports = { run, register, channel, gate, busy, homeDir, dataDir, cacheDir };
