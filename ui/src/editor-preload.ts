// editor-preload.ts
// 插件「独立编辑器子窗口」preload（frame:false 无边框窗口）。
// 编译产物 build/editor-preload.js 由 main.ts 的 win:openPluginWindow 在 frameless 子窗口注入。
// 作用：仅暴露最小窗口控制桥 octWin（min/max/close + 最大化状态监听），
//       让子页自绘标题栏（editor.html 的 .titlebar）可拖拽并控制窗口。
// 注意：子窗口 contextIsolation:true、nodeIntegration:false，绝不在此暴露完整 node/ipcRenderer。
import { contextBridge, ipcRenderer, IpcRendererEvent } from "electron";

contextBridge.exposeInMainWorld("octWin", {
  min: () => ipcRenderer.send("win:ctrl", "min"),
  max: () => ipcRenderer.send("win:ctrl", "max"),
  close: () => ipcRenderer.send("win:ctrl", "close"),
  // 复制文本：字幕窗经常处于失焦态，渲染侧 navigator.clipboard.writeText 会被 Chromium 拒绝；
  // 走主进程 clipboard:write（与焦点无关），作为渲染复制失败时的兜底。
  copyText: (text: string): Promise<{ ok: boolean; error?: string }> =>
    ipcRenderer.invoke("clipboard:write", { text }),
  // 字幕子窗「翻译中调整区域」：请求主进程开/关全屏 interactive overlay（如 translate_overlay）。
  // 与主面板 postMessage oct.overlay 走同一个 overlay:open/close IPC。
  // persistent=true：常驻金边模式，开窗后不自动关闭，由页面在 pick/live 两阶段间切换。
  openOverlay: (pluginId: string, persistent?: boolean): Promise<unknown> =>
    ipcRenderer.invoke("overlay:open", { pluginId, interactive: true, persistent: !!persistent }),
  closeOverlay: (pluginId: string): Promise<unknown> =>
    ipcRenderer.invoke("overlay:close", { pluginId }),
  // 已开着的常驻框：切换阶段（pick=拉框模态 / live=金边常驻）；窗口不存在时返回 {ok:false}。
  overlayPhase: (pluginId: string, phase: string): Promise<{ ok: boolean; error?: string }> =>
    ipcRenderer.invoke("overlay:phase", { pluginId, phase }),
  // 订阅最大化/还原状态（主进程 maximize/unmaximize 时推送 win:state）。
  // 返回取消订阅函数，便于页面卸载时解绑。
  onMaxState: (cb: (state: string) => void): (() => void) => {
    const listener = (_e: IpcRendererEvent, s: string) => {
      try { cb && cb(s); } catch (_) { /* 页面回调异常忽略 */ }
    };
    ipcRenderer.on("win:state", listener);
    return () => { ipcRenderer.removeListener("win:state", listener); };
  },
});