"""oct_sdk —— OCTplugins Python 插件 SDK（§16.5 / §11.2–§11.4 / §10.5）。

契约：
  - fd 1 自进程启动起为协议专用流（NDJSON），语言层默认输出改道 fd 2（§11.2）；
  - win32 平台 MUST 执行 stdin/stdout reconfigure(encoding="utf-8")（§11.2.1）；
  - 启动后立即发 $/handshake（token 取自 env OCT_CHANNEL_TOKEN）（§11.4）；
  - 在独立线程响应内核请求（ping 不阻塞业务逻辑，§13.3）；
  - busy 模式经上下文管理器 sdk.busy(max_ms=...) 申请/释放（§10.5）。

强制接口（§16.5）：run / home_dir / data_dir / cache_dir / busy / channel.call / gate。
"""

import json
import os
import sys
import threading
import queue

from .protocol_errors import CODES, DIAG_BY_CODE, MESSAGES  # §19 派生，勿硬编码错误码

__version__ = "0.1.0"

# JSON-RPC 2.0 通用「内部错误」（非 §19 插件域私有表，故保留本地常量）。
_ERR_INTERNAL = -32603

_PROTOCOL_VERSION = 1


class ProtocolError(Exception):
    """携带 §19 错误码的业务异常：SDK 直接回该错误帧，不降级为 E_INTERNAL。

    插件用 `raise ProtocolError("E_TOOL_UNAVAILABLE", "...")` 表达「外部可执行文件缺失」
    这类语义化失败，宿主/调用方据此得到准确 code 与 diag。
    """

    def __init__(self, diag, message=None, data=None):
        self.diag = diag
        self.code = CODES.get(diag, _ERR_INTERNAL)
        self.message = message or MESSAGES.get(diag, diag)
        self.data = dict(data) if data else {}
        super().__init__(self.message)

# ── fd1 接管（模块导入即生效，§11.2 首行接管） ────────────────────────
def _capture_stdout():
    """接管语言层默认输出：fd1 留给协议，sys.stdout 改道 stderr。"""
    global _proto_fd
    # 直接包 fd1 原始字节流（协议专用），不经过 sys.stdout。
    _proto_fd = os.fdopen(1, "wb", buffering=0)
    sys.stdout = sys.stderr
    if sys.platform == "win32":
        # §11.2.1：管道 stdout 默认 locale 编码（cp936），必须显式 utf-8。
        sys.stdin.reconfigure(encoding="utf-8")
        sys.stdout.reconfigure(encoding="utf-8")
    return _proto_fd


_proto_fd = _capture_stdout()


# ── 路径访问器（§5.2） ─────────────────────────────────────────────
def home_dir():
    """插件包目录（只读）：OCT_PLUGIN_HOME。"""
    return os.environ.get("OCT_PLUGIN_HOME") or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def data_dir():
    """插件数据目录（可写）：OCT_PLUGIN_DATA = state/plugins/<id>/。一切持久化写入必须在此内。"""
    return os.environ.get("OCT_PLUGIN_DATA") or os.path.join(home_dir(), "data")


def cache_dir():
    """缓存目录：数据目录下的 cache 子目录（宿主未单独注入，§5.2 表内无 OCT_PLUGIN_CACHE）。"""
    d = os.path.join(data_dir(), "cache")
    os.makedirs(d, exist_ok=True)
    return d


# ── 帧 I/O ──────────────────────────────────────────────────────────
_next_id = 0
_id_lock = threading.Lock()
_pending = {}


def _next_request_id():
    global _next_id
    with _id_lock:
        _next_id += 1
        return _next_id


_write_lock = threading.Lock()  # §12.2：写操作串行化，避免多线程半帧交错


def _write(msg):
    """写一条 NDJSON 到 fd1（标准序列化器，禁止 pretty-print）。持锁保证原子写。"""
    with _write_lock:
        _proto_fd.write(json.dumps(msg, ensure_ascii=False).encode("utf-8"))
        _proto_fd.write(b"\n")


def _post(method, params):
    """发出请求并阻塞等响应（供 channel.call / gate 用）。"""
    rid = _next_request_id()
    q = queue.Queue(maxsize=1)
    _pending[rid] = q
    _write({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
    resp = q.get()  # 读循环线程投递
    _pending.pop(rid, None)
    if resp.get("error"):
        raise RuntimeError(resp["error"])
    return resp.get("result")


# ── 通道与 gate（§11.6 / §16.5） ──────────────────────────────────
class _Channel:
    def call(self, name, params=None):
        """跨插件共享函数调用（内核中转，§11.6）。"""
        return _post("registry.call", {"name": name, "params": params})

    def emit(self, typ, data=None):
        """向宿主广播事件。"""
        return _post("event.emit", {"type": typ, "data": data})

    def busy(self, max_ms):
        """进入 busy（§10.5），应在独立线程使用；返回 None。"""
        _post("sdk.busy", {"maxBusyMs": max_ms})


class _Gate:
    def read_file(self, path):
        return _post("gate.file_read", {"perm": "file_read", "path": path})

    def file_exists(self, path):
        return _post("gate.file_exists", {"path": path})

    def write_file(self, path, data):
        """受 file_write 权限保护的写文件（覆盖写入）。"""
        return _post("gate.file_write", {"path": path, "data": data})

    def exec(self, command, args=None, timeout_ms=15000):
        """受 execute_command 权限保护的命令执行（不经过 shell，防注入）。"""
        return _post("gate.execute_command",
                     {"command": command, "args": args or [], "timeoutMs": int(timeout_ms)})


channel = _Channel()
gate = _Gate()


class busy:
    """§10.5 上下文管理器：进入/退出 busy 自动成对，无需作者手工 enter/exit。"""

    def __init__(self, max_ms):
        self.max_ms = int(max_ms)

    def __enter__(self):
        _post("sdk.busy", {"maxBusyMs": self.max_ms})
        return self

    def __exit__(self, exc_type, exc, tb):
        _post("sdk.busy", {"maxBusyMs": 0})
        return False


# ── 主循环（独立线程：保证 ping 独立应答，§13.3） ──────────────────
_handlers = {}       # method → fn(params) -> result
_shutdown_evt = threading.Event()


def _handle_request(req):
    """处理内核发来的请求并回响应。"""
    rid = req.get("id")
    method = req.get("method")
    params = req.get("params") or {}
    try:
        if method == "ping":
            result = {"pong": True}  # 心跳应答（§13.3，任何协议行也是活性证据）
        elif method == "shutdown":
            _shutdown_evt.set()  # 优雅关闭信号；进程随后被内核终止
            result = {"ok": True}
        else:
            fn = _handlers.get(method)
            if fn is None:
                raise KeyError(method)
            result = fn(params)
        _write({"jsonrpc": "2.0", "id": rid, "result": result})
    except Exception as e:  # noqa: BLE001 —— 协议错误必须回错误帧而非崩溃
        # §19 派生：错误码/诊断码取 pkg/protocol，不硬编码。
        if isinstance(e, ProtocolError):
            _err = {"code": e.code, "message": e.message,
                    "data": dict({"diag": e.diag}, **e.data)}
        elif isinstance(e, KeyError):  # 未知 method
            _err = {"code": CODES["E_METHOD_NOT_FOUND"], "message": "unknown method", "data": {"diag": "E_METHOD_NOT_FOUND"}}
        else:  # 业务/IO 等通用内部错误
            _err = {"code": _ERR_INTERNAL, "message": str(e), "data": {"diag": "E_INTERNAL"}}
        _write({"jsonrpc": "2.0", "id": rid, "error": _err})


def _read_loop():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except ValueError:
            continue  # 协议违规行由内核侧 Deframer 负责暴露（本进程的 stdout 才是协议流）
        if isinstance(msg, dict) and "method" in msg:
            m = msg.get("method")
            if m in ("ping", "shutdown"):
                # 快路径：读循环线程直接应答，绝不被慢 handler 阻塞（§13.3）
                _handle_request(msg)
            else:
                # 慢路径：业务 handler 交给独立 worker 串行执行，读循环持续读流，
                # 保证业务阻塞（如本地进程探测数秒）时不饿死 ping 应答。
                _handler_queue.put(msg)
            continue
        if isinstance(msg, dict) and "id" in msg:
            q = _pending.get(msg["id"])
            if q is not None:
                q.put(msg)


_handler_queue = queue.Queue()


def _dispatch_worker():
    while True:
        req = _handler_queue.get()
        if req is None:
            return
        _handle_request(req)


threading.Thread(target=_dispatch_worker, daemon=True).start()


def register(method):
    """注册插件方法（宿主经 plugin.call 调用，§7 functions）。"""

    def deco(fn):
        _handlers[method] = fn
        return fn

    return deco


def run(main=None, args=None):
    """§16.5 sdk.run(handler)：
      1) 首帧 $/handshake（token 经 env 注入，§11.4）；
      2) 启动独立线程读循环（ping 独立应答，§13.3）；
      3) 在主线程执行 main(channel, args)；随后阻塞至 shutdown/被内核终止。
      阻塞语义：插件进程由内核监管（§12.2 谁派生谁回收），run 不自行限时退出，
      以免常驻插件（resident）在无请求时 30s 后自绝，进程只在内核 Stop 时结束。
    """
    token = os.environ.get("OCT_CHANNEL_TOKEN", "")
    _write({"jsonrpc": "2.0", "method": "$/handshake",
            "params": {"protocolVersion": _PROTOCOL_VERSION, "token": token}})
    t = threading.Thread(target=_read_loop, daemon=True)
    t.start()
    if callable(main):
        main(channel, args)
    elif main is not None and isinstance(main, dict):
        # 兼容 dict 风格 handler：{ "main": fn, "methods": {...} }
        for k, v in main.get("methods", {}).items():
            _handlers[k] = v
        m = main.get("main")
        if callable(m):
            m(channel, args)
    _shutdown_evt.wait()  # 阻塞至内核 shutdown/终止
    return True
