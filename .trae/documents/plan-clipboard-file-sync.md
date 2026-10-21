# 剪贴板同步 + 手动文件传输 实施计划

## Context

OctPlugins 需要连接一台**用户可配置**的远程服务器（默认 `http://74.176.161.29:8080`），实现：

1. **剪贴板自动同步（双向）**：本机复制内容 → 主动上传到服务器共享文件；客户端**定时轮询**服务器比对「版本 hash」（= 剪贴板内容 sha256），不一致就下载并写回本机剪贴板。用户原话：「每次剪切板更新放一个版本号，只需要检测版本hash是否一致」。
2. **手动文件传输**：UI 中选择文件上传、列出服务器文件、下载到本地（下载走保存对话框）。

服务器端当前为空，需要**连一个 Go 服务器程序一起写**（纯 stdlib，部署到 74.176.161.29）。

**已定架构**（经探查与验证）：
- 同步引擎放 **Electron 宿主主进程 `ui/src/main.ts`**（Electron 31 / Node 20 全局 fetch；Electron clipboard.readText/writeText 可靠；main.ts 已有 setInterval 先例 L763、clipboard:write IPC L69-76）。不放 Go 插件：Go 侧剪贴板需额外依赖且有 Windows 消息泵坑。
- 新插件 **`plugins/sync/`**：type Go + entry sync.exe **极简 stub**（load_mode lazy，永不被 spawn，仅满足内核契约校验；已确认 contract.go L37-85 不校验 entry 存在、lazy 不启动，见 Phase 1 验证）；UI 负责状态/文件传输/快捷开关。
- 配置走 **settingsSchema**：`plugin.getSettings/setSettings`（kernel/internal/ipcserver/ws.go L347-422）落 `config/user-settings.json`；宿主设置面板 `renderPluginSettings`（ui/src/renderer.ts L953-1012）**自动渲染** string/boolean/integer 控件并保存——sync 的配置项零成本获得编辑 UI。
- 新插件目录放入 `plugins/` 重启内核即自动注册（manager.go RegisterAll L730 / Discover L452），**无需**处理 registry manifestHash。

## 文件清单

### 新建
| 文件 | 职责 |
|---|---|
| `mvp_test/syncserver/main.go` + `go.mod` | Go 服务器（接口契约见下），纯 stdlib |
| `mvp_test/syncserver/build.bat` | `go build -o syncserver.exe .` |
| `plugins/sync/manifest.json` | type Go、entry sync.exe、load_mode lazy、ui web、settingsSchema、defaults、permissions `["network"]` |
| `plugins/sync/main.go` | stub：打印后常驻 sleep（永不被 spawn） |
| `plugins/sync/build.bat` | `go build -o sync.exe .` |
| `plugins/sync/ui/index.html` + `app.js` | 状态卡 + 文件传输卡 + 快捷开关；复用 oim UI 样式（见下） |

### 修改
| 文件 | 改动 |
|---|---|
| `ui/src/main.ts` | 同步引擎（双定时器状态机）+ `ipcMain.handle("sync:cfg" \| "sync:files:list" \| "sync:files:upload" \| "sync:files:download")` + `win.webContents.send("sync:status", ...)` |
| `ui/src/renderer.ts` | 转发 `oct.sync.*` 消息（iframe→main，仿 `oct.clipboard.write` L281-284 的 `ipcRenderer.invoke` 写法）；`ipcRenderer.on("sync:status")` → `forwardToPlugin("sync", {type:"oct:sync:status",data})`（仿 `kernel:event` L1292-1318 + forwardToPlugin L304-314） |

## 服务器接口契约（Go stdlib，`-addr :8080 -dir <datadir> -token <可选>`）

- `PUT /clipboard` body=text → 200 `{"hash":"<sha256>","updatedAt":"RFC3339","size":n}`；上限 1MB；原子写 `data/clipboard.txt`
- `GET /clipboard/meta` → 200 `{"hash","updatedAt","size"}`；无内容 404 `{"error":"empty"}`
- `GET /clipboard` → 200 text/plain；无内容 404
- `GET /files/` → 200 `[{"name","size","hash","updatedAt"}]`（按名排序）
- `PUT /files/{name}` → body 原始字节，name 取 `filepath.Base`（拒绝 `.`/空/含分隔符）；上限 64MB；200 `{"name","hash","size"}`
- `GET /files/{name}` → `Content-Disposition: attachment` 下载；不存在 404
- 通用：全局 mutex + tmp/rename 原子写；所有响应带 `Access-Control-Allow-Origin:*` + OPTIONS 预检；`-token` 非空时校验 `X-Sync-Token` 头，不符 401

## 宿主引擎（main.ts）

状态：`{ enabled, serverUrl, token, lastLocalText, lastServerHash, online, lastErr }`

- **启动**：`lastLocalText = clipboard.readText()`（首读仅播种、**不上传**）；服务器轮询每个 tick 先 `rpc("plugin.getSettings", {pluginId:"sync"})` 刷新配置（单一事实源）。
- **定时器 A 400ms（剪贴板轮询）**：`t = clipboard.readText()`；若 `t !== lastLocalText && t !== "" && enabled && online` → `PUT /clipboard` 上传，`lastServerHash = 返回hash`，`send sync:status`；无论是否上传，`lastLocalText = t` 恒在比较后更新。
- **定时器 B 3s（服务器轮询）**：`busy` 防重入；`GET /clipboard/meta`：
  - 404 → `lastServerHash = ""` 后跳过（服务器无内容不下载）；
  - hash === lastServerHash → 跳过；
  - 不一致 → `GET /clipboard` 全文，若 `!== clipboard.readText()` 则 `clipboard.writeText(content)` **并立即 `lastLocalText = content`**（防回环关键），`lastServerHash = hash`，`send sync:status`。
- 所有 fetch 带 AbortController 5s 超时；离线 → `online=false` + status，每 tick 重试，不抛异常不挂死。
- 文件传输 IPC（main 进程完成 HTTP + dialog/fs）：`sync:files:list` → GET /files/；`sync:files:upload` → `dialog.showOpenDialog` 选文件 → PUT；`sync:files:download` → GET → `dialog.showSaveDialog` → 写盘。

## 防回环 / 边界条件

- 写回剪贴板后立即更新 `lastLocalText`，避免本地轮询再上传同一内容；
- 上传后记录 `lastServerHash`，避免 3s 后自下载自己上传的内容；
- 空内容不上传（避免清剪贴板触发抖动）；
- meta 404 视为服务器空，不下载；
- 服务器离线：状态显示离线、每 tick 重试、不崩；
- 多设备并发：last-write-wins（谁后 PUT 谁赢），单一共享文件；
- token 401：状态显示鉴权错误，停止上传直到配置变更；
- 大内容/大文件：1MB / 64MB 上限，超限提示。

## settingsSchema（manifest.json 内）

```json
"settingsSchema": { "type": "object", "properties": {
  "serverUrl": { "type": "string", "title": "服务器地址", "default": "http://74.176.161.29:8080" },
  "token":     { "type": "string", "title": "共享密钥（可空）", "default": "" },
  "enabled":   { "type": "boolean", "title": "开启剪贴板同步", "default": false },
  "pollMs":    { "type": "integer", "title": "拉取间隔(ms)", "default": 3000 }
}},
"defaults": { "serverUrl": "http://74.176.161.29:8080", "token": "", "enabled": false, "pollMs": 3000 }
```

## UI（plugins/sync/ui/index.html）

复用 oim 样式（plugins/oim/ui/index.html L7-11）：`--bg:#f6f4ef; --card:#fff; --line:#e9e4da; --ink:#221f1a; --gold:#c8a15c; --gold-deep:#8f6f3d`，卡片 `.card`，主按钮金色渐变（用户偏好暗金非绿）。三块：
1. **状态卡**：服务器在线/离线、当前 hash、上次同步时间、最后错误；
2. **快捷开关**：剪贴板同步 enable（调 `plugin.setSettings`）；
3. **文件传输卡**：`选择文件上传` 按钮、服务器文件列表（名称/大小/时间）、每行 `下载` 按钮。

## 验证

1. 本地服务器：`cd mvp_test/syncserver && go build -o syncserver.exe .`，`.\syncserver.exe -addr :8080`；
2. curl：`curl.exe -X PUT -d hello http://127.0.0.1:8080/clipboard` → 得 hash；复查 `/clipboard/meta`、`/clipboard`；文件 `curl.exe -T` PUT/GET；未上传前 meta 应 404；
3. 部署公网：scp syncserver.exe 到 74.176.161.29，开放 8080，`./syncserver.exe -addr :8080 -dir /srv/syncdata`；同 curl 打公网 IP；
4. `plugins/sync` 下跑 build.bat 产 sync.exe；杀残留 electron（`Get-Process electron | Stop-Process -Force`）→ `cd ui && npm start` → 内核重启自动注册 sync，插件列表可见，设置面板出现 sync 配置项；
5. 端到端双设备：A 复制 "foo" → B 3s 内粘贴得 "foo"；B 复制 "bar" → A 得 "bar"；杀掉服务器 → 状态变离线不崩；恢复后自动继续同步；上传/下载任意文件验证文件传输。
