"""模型加载权的 tool 侧实现（conversion.md §四 加载权硬约束 / §19.6）。

硬约束：
  - **只有宿主**可加载 / 卸载模型（内核 ResourceMap.Acquire / Release）；
  - **tool / outtool 同样不得加载模型**：只能请求宿主加载；
  - tool / outtool 内部只做**推理**，不做权重拉取、不做显存调度。

因此 tool 侧提供两个方向的接口：

1. **宿主指令入口**（由内核发起）：
     - ``model.ensure {modelId}`` → 真正构造运行时对象并缓存；
     - ``model.release {modelId}`` → 丢弃缓存，释放运行时占用（不删盘）。
   这两个方法经 ``install_handlers`` 统一注册，tool 作者只需提供 ``loaders``。

2. **推理路径取用**：
   ``acquire(model_id, loader)`` —— 已加载则复用；未加载时按 §19.6 报
   ``E_MODEL_NOT_READY``，而不是偷偷自行加载。

逃生阀（仅供本机调试，不参与宿主链路）：设置环境变量
``OCT_ALLOW_TOOL_SELFLOAD=1`` 可让推理路径在未 ensure 时自行加载，
用于绕过宿主直接 ``registry.call`` 调试单个 tool。生产路径下宿主会先 ensure。
"""

import os
import threading
import time

SELFLOAD_ENV = "OCT_ALLOW_TOOL_SELFLOAD"

_LOADED = {}          # modelId -> 运行时对象（recognizer / tts / llm / translator / session...）
_LOADED_MS = {}       # modelId -> 加载耗时（毫秒），供状态视图展示
_LOCK = threading.Lock()


def selfload_allowed():
    """是否允许 tool 自行加载（逃生阀，默认关）。"""
    return os.environ.get(SELFLOAD_ENV, "").strip().lower() in ("1", "true", "yes", "on")


def loaded_ids():
    """已由宿主指令加载的模型 id 列表。"""
    with _LOCK:
        return sorted(_LOADED)


def is_loaded(model_id):
    with _LOCK:
        return model_id in _LOADED


def get(model_id):
    with _LOCK:
        return _LOADED.get(model_id)


def ensure(model_id, loader, path=None):
    """宿主指令下的加载（幂等）。返回 ``(obj, created)``。

    ``loader(path)`` 由 tool 作者提供，负责真正构造运行时对象（可能耗时数秒到数十秒）；
    在锁外执行，避免阻塞其他模型的加载指令。

    ``path`` 由宿主下发（§1.4：tool 只按 id 申请，路径由宿主给）——
    这样模型被搬迁到 <modelId>/<quant>/ 规范布局后，tool 仍能拿到正确位置。
    tool 应优先使用该 path，为空时才回落自身 manifest 默认值。
    """
    if not model_id:
        raise ValueError("modelId required")
    with _LOCK:
        if model_id in _LOADED:
            return _LOADED[model_id], False
    started = time.time()
    obj = loader(path)
    elapsed_ms = int((time.time() - started) * 1000)
    with _LOCK:
        # 并发 ensure 同一模型时保留先到的对象。
        if model_id not in _LOADED:
            _LOADED[model_id] = obj
            _LOADED_MS[model_id] = elapsed_ms
        return _LOADED[model_id], True


def release(model_id):
    """宿主指令下的卸载：丢弃缓存对象（权重文件保留，§14.7 MUST NOT 删盘）。"""
    with _LOCK:
        existed = _LOADED.pop(model_id, None) is not None
        _LOADED_MS.pop(model_id, None)
        return existed


def release_all():
    with _LOCK:
        ids = sorted(_LOADED)
        _LOADED.clear()
        _LOADED_MS.clear()
        return ids


def load_ms(model_id):
    with _LOCK:
        return _LOADED_MS.get(model_id)


def acquire(model_id, loader):
    """推理路径取用模型：已加载 → 复用；未加载 → 按 §19.6 报错（或走逃生阀自加载）。"""
    with _LOCK:
        if model_id in _LOADED:
            return _LOADED[model_id]
    if not selfload_allowed():
        from . import ProtocolError

        raise ProtocolError(
            "E_MODEL_NOT_READY",
            "模型 %s 尚未由宿主加载（§19.6：tool 不得自行加载模型）。"
            "请先经宿主 registry.models.acquire 或 gate.model_ensure 确保就绪；"
            "本机调试可用 OCT_ALLOW_TOOL_SELFLOAD=1 绕过。" % model_id,
        )
    # 逃生阀：无宿主下发路径，用 tool 自身默认（loader(None)）。
    obj, _ = ensure(model_id, loader, None)
    return obj


def install_handlers(register, loaders, unloaders=None, status_extra=None):
    """为 tool 注册标准的 ``model.ensure`` / ``model.release`` / ``models`` 三个方法。

    :param register: ``oct_sdk.register``
    :param loaders: ``{modelId: (path) -> 运行时对象}``。``path`` 为宿主下发的权重路径
                    （可能为 None，此时 tool 应回落自身默认值）。
    :param unloaders: ``{modelId: () -> None}`` 可选，用于需要显式销毁的运行时
                      （如先 close 再释放）；缺省仅丢引用。
    :param status_extra: ``() -> dict`` 可选，向 ``models`` 结果补充 tool 自有的
                         在位性/版本信息（失败时忽略，不影响主结果）。
    """
    unloaders = unloaders or {}

    def _known():
        return sorted(loaders)

    @register("model.ensure")
    def _model_ensure(params):
        p = params if isinstance(params, dict) else {}
        mid = str(p.get("modelId") or p.get("id") or "").strip()
        if not mid:
            from . import ProtocolError

            raise ProtocolError("E_PARSE", "model.ensure: modelId required")
        loader = loaders.get(mid)
        if loader is None:
            from . import ProtocolError

            raise ProtocolError(
                "E_MODEL_UNAVAILABLE",
                "本 tool 不提供模型 %s（可提供：%s）" % (mid, ", ".join(_known()) or "(无)"),
            )
        # §1.4：宿主下发权重路径；tool 优先用它，为空才回落自身默认。
        path = str(p.get("path") or "").strip() or None
        companion = p.get("companionPaths") if isinstance(p.get("companionPaths"), dict) else {}
        ensure(mid, lambda _p: loader(path, companion), path)
        return {
            "ok": True,
            "modelId": mid,
            "loaded": True,
            "path": path or "",
            "ms": load_ms(mid),
        }

    @register("model.release")
    def _model_release(params):
        p = params if isinstance(params, dict) else {}
        mid = str(p.get("modelId") or p.get("id") or "").strip()
        if not mid:
            from . import ProtocolError

            raise ProtocolError("E_PARSE", "model.release: modelId required")
        hook = unloaders.get(mid)
        if hook is not None:
            try:
                hook()
            except Exception:  # 卸载失败不应让宿主卡死，记录后继续丢引用
                pass
        return {"ok": True, "modelId": mid, "released": release(mid)}

    @register("models")
    def _models(_params):
        out = {
            "loaded": loaded_ids(),
            "available": _known(),
            "loadMs": {m: load_ms(m) for m in loaded_ids()},
            "selfLoadAllowed": selfload_allowed(),
        }
        if status_extra is not None:
            try:
                out.update(status_extra() or {})
            except Exception:  # 诊断信息失败不应让状态查询整体失败
                pass
        return out
