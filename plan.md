# OctPlugins 修复方案(详细版)

> 依据:`Problem-20261025.md`(2026-10-25 全盘扫描 + 23 插件逐一评审)。编号沿用:P0-x/P1-x/P2-x/P3-x = 主仓库问题;§8.3-N = 插件问题;`旧P0-x` = 10-13 旧报告仍未修条目。
> 性质:只给方案不改代码。每个条目含:**问题 → 方案(步骤分解)→ 涉及位置 → 边界与风险 → 验收 → 回滚**。
> 前置事实(执行前需现场确认的两点,影响 0.4 的做法):
> ① 根目录 `plugins/` 与 `mvp_test/plugins/` 同名 23 插件并存(前者 161MB,后者为开发树)——需确认内核实际发现目录(看 `state/registry.json` 的路径字段或 config),**以实际加载的目录为"正本"**;
> ② `kernel/kerneld.exe` 当前是 10-21 前的旧产物,阶段 0 修复编译后须先验证再替换。

---

## 0. 阶段总览与依赖关系

| 阶段 | 目标 | 覆盖 | 人日 | 前置 |
|---|---|---|---|---|
| 0 止血 | 可构建 + P0 清零 + 凭据应急 | P0-1~5、§8.3-10/15 | 1.5 | 无 |
| 1 安全 | 凭据出库/窗口基线/桥收口/静态鉴权 | P1-3~6、§8 安全项 | 3.5 | 0.4 未入库前完成 1.5 |
| 2 协议 | 错误码单源/sdk-go/双 SDK 对齐/busy | P1-1/2、§8.2-1/3/7 | 4 | 0.1 |
| 3 内核正确性 | 竞态/事件序/假实现决断/死配置 | P1-7、P0-2、P2-1~3 | 5 | 0.1 |
| 4 插件清账 | 23 插件三波修复 | §8 全部 | 6.5(与 3 并行) | 阶段 2 的 sdk-go 先行 |
| 5 工程化 | CI/lint/strict/提交规范/卫生断言 | P3 全部 | 2.5 | 0.4(否则卫生断言无对象) |
| 6 架构收敛 | UI 拆分/设置单源/文档校验/瘦身 | P2-4、P3-3/5、R6 | 持续切片 | 阶段 1 的 preload 桥 |

依赖链:`0.1 → (2,3) → 4`;`0.4 → 5`;`1.1 的 preload 桥 → 6.1`;其余可并行。

---

## 阶段 0:止血

### 0.1 修复 kernel/internal/runtime 编译断裂 【P0-1】

**问题**:`go build ./...` 实测失败。runtime 包被拆分为 runtime.go/python.go/node.go/lock.go/provider.go 的重构半途中断:`runtime.go` 仍保留 8 个旧方法与拆分文件重复声明,`lock.go:62,67` 调用不存在的 `GenerateLock`。

**步骤**:
1. 逐个删除 `runtime.go` 中的 8 个旧方法(保留拆分文件版本):
   | 旧方法(删除) | 位置 | 正本(保留) |
   |---|---|---|
   | `Installer.VenvPython` | runtime.go:133 | python.go:142 |
   | `Installer.NodeInterp` | runtime.go:163 | node.go:103 |
   | `Installer.DepsPlan` | runtime.go:186 | lock.go:89 |
   | `Installer.BundledPythonDir` | runtime.go:215 | python.go:90 |
   | `Installer.BundledPython` | runtime.go:226 | python.go:96 |
   | `Installer.ManagedPython` | runtime.go:251 | python.go:294 |
   | `Installer.ListPackages` | runtime.go:259 | python.go:313 |
   | `Installer.NodePackages` | runtime.go:264 | node.go:126 |
2. 删除前先 diff 新旧实现:若旧方法体里有新文件没有的逻辑(如缓存、锁),把差异合并进正本,不整段盲删。
3. 补 `GenerateLock`:先读 `runtime_test.go` 与 `lock.go:62,67` 的调用形态确定签名(预期 `func (i *Installer) GenerateLock(...) (string, error)`,返回依赖清单内容的 sha256 十六进制);实现放 `lock.go`,逻辑 = 收集 manifest 声明的依赖文件(requirements.txt/package.json 等)→ 按路径排序 → 逐文件读内容(不存在则返回错误,不得静默跳过)→ 拼接哈希。
4. `go build ./... && go vet ./... && go test ./internal/runtime/...` → 全仓测试。
5. 重编内核:`go build -o kerneld.new.exe ./cmd/kerneld`(在工作区,**不入 git**);备份现行 `kerneld.exe` 为工作区副本(不入 git);替换后跑 `node tools/e2e-smoke.mjs`(该脚本会先删 registry.json 重建基线,注意先备份 `state/`)。
6. e2e 通过后,用新 exe 覆盖 git 跟踪的 `kernel/kerneld.exe`(0.3 的入库策略),commit 注明构建 commit 哈希与 Go 版本。

**边界**:
- 拆分文件本身可能有半成品问题(vet 只报了重复声明与 GenerateLock,但 `too many errors` 截断了输出)——修完前两类后重跑 vet 直到零截断;
- 若 `provider.go` 也参与拆分,同步检查其与 runtime.go 的符号冲突。

**验收**:`go build ./...`、`go vet ./...`、`go test ./...` 三连绿;e2e-smoke 通过;宿主能拉起新内核并完成 plugin.list。

**回滚**:恢复工作区备份的旧 kerneld.exe;源码回退 commit。

### 0.2 命令面板崩溃修复 【P0-5】

**问题**:`ui/src/renderer.ts:1520` 对无 `params` 字段的 `SYS_CMDS` 条目执行 `p.params.length` 抛 TypeError,面板空白。

**步骤**:
1. `1520` 行改为 `${(p.params || []).length ? ... : ""}`;
2. 给 `SYS_CMDS` 数组每一条补 `params: []`(类型上对齐 PALETTE 条目,防止同类回归);
3. 顺带删 `renderer.ts:1009` 的空表达式 `+ (eff.auto_restart ? "" : "")`(旧P2-33,零风险)。

**验收**:Ctrl+K 混合渲染系统命令与插件命令;浏览器 console 无 TypeError。

### 0.3 git 仓库去污染 【P0-4】

**步骤**(命令按序执行):
```
git rm --cached kernel/kerneld.new.exe kernel/kerneld.exe.bak-20261013-152326 \
  kernel/kerneld.exe.bak-20261021-131932 kernel/kerneld.exe.inuse-20261013-152457
git rm -r --cached kernel/kernel/
git rm --cached _home.html _state.json screen_full.png
```
工作区对应文件直接删除(`_home.html`/`_state.json` 与项目无关;截图无用)。

`.gitignore` 增补(放在 "# 备份文件" 一节附近):
```
kernel/kerneld.new.exe
kernel/kerneld.exe.bak-*
kernel/kerneld.exe.inuse-*
kernel/kernel/
*.exe~
screen_full.png
```

**入库策略决定**:`kernel/kerneld.exe` **继续跟踪**(build.ps1 的回退保障 + 项目无 CI 产物分发)。在 `kernel/README.md`(新建,3 行)记录:构建命令、当前 exe 对应的源码 commit、Go 版本。

**验收**:`git ls-files | grep -iE "\.exe|\.bak|inuse"` 仅剩 `kernel/kerneld.exe`;`git status` 干净;`git check-ignore kernel/kerneld.new.exe` 命中。

**不做**(挪到阶段 5.3):历史中的 ~57MB exe 用 filter-repo 清除——需先备份 `.git`,单人仓库可直接做,做完后远端强推。

### 0.4 插件源码入库 【P0-3】

**前置确认**:看 `state/registry.json` 各插件条目的路径字段,确认内核加载的是根 `plugins/` 还是 `mvp_test/plugins/`。**下文以"正本目录"指代实际加载者,另一目录定义为部署副本**。

**步骤**:
1. **先清理正本目录**(逐项删/移,清单来自 §8.2-6):
   | 目录 | 动作 |
   |---|---|
   | 所有插件 | 删 `*.exe~`、`__pycache__/`(12 处) |
   | colorpicker | `native/target/` 整目录删(176MB→~2MB);`color_state.json` 删(运行数据) |
   | hardware | 删无扩展名二进制 `hardware`(3.1MB,非 exe 但同产物) |
   | home | 删 `net.ps1 - 快捷方式.lnk`、`open_work_*_temp.bat`、`home_state.json`;补齐 `net.ps1` 本体(从 lnk 的目标路径找回,或按 net_windows.go 的调用参数重写) |
   | mdeditor | 删 `test.md`、`mdeditor_state.json` |
   | proxy | `profile_config/default.yaml` 改名 `default.yaml.example` 并**脱敏**(替换所有 uuid/password 为占位符)——真实凭据移到 `state/plugins/proxy/`(见 1.5) |
   | data-convert 等的 `start.lock` | 保留文件但加入忽略规则(它是运行期产物) |
2. **每个插件目录放一份局部 `.gitignore`**(模板,可先写一份 `tools/plugin-gitignore-template` 由脚本拷入):
   ```
   *.exe
   *.exe~
   *.dll
   __pycache__/
   *.pyc
   native/target/
   node_modules/
   .venv/
   *_state.json
   start.lock
   profile_config/*.yaml
   !profile_config/*.example
   *.onnx
   *.gguf
   ```
   例外申请(显式放行):colorpicker/colorpicker.exe 若希望源码即用可暂保留跟踪——**建议不保留**,统一走构建(build.bat 已在,补一个 `tools/build-all-plugins.ps1` 顺序调各 build.bat)。
3. **mdeditor 的 `ui/lib/vs/`**(25MB monaco)**本轮保留入库**:它是运行必需的 vendored 依赖且无 CDN 依赖(离线宿主),瘦身放阶段 6.4。同理 proxy 的 `resources/mihomo.exe`(59MB)保留但记录在 6.4 评估压缩/按需下载。
4. **根 `.gitignore` 改造**:把 `/plugins/`、`mvp_test/` 两条整目录忽略改为:
   ```
   /plugins/
   !mvp_test/plugins/
   mvp_test/plugins/**/          # 子目录默认忽略
   !mvp_test/plugins/*/          # 放行插件目录本身(源码由局部 .gitignore 细控)
   ```
   (具体写法以 `git check-ignore -v` 逐个验证为准;目标 = 源码入、产物不入。)
5. 若正本 = 根 `plugins/`:同理放行根目录源码;`mvp_test/plugins` 保持忽略,并在 build.ps1 加一步"正本 → mvp_test 同步"(或反之,方向取决于第 1 步结论)。
6. `git add` 后先跑一遍**凭据扫描**(见验收)再 commit。

**验收**:
- `git ls-files mvp_test/plugins | wc -l` > 100 且 `git ls-files | grep -E "\.exe$|__pycache__|\.onnx$|target/"` 为空(或仅白名单);
- 凭据扫描:`git grep -iE "uuid:|password:|[0-9a-f]{32}" -- mvp_test/plugins` 人工复核命中项全部为占位符/example;
- 换机演练(可选但强烈建议):clone 到新目录 → build.ps1 → start.bat,插件列表完整。

**回滚**:整体在一个 commit,`git revert` 即可。

### 0.5 home 凭据应急轮换(运维) 【§8.3-10】

**步骤**:
1. 在 `74.176.161.29:8787` 服务端使 `cd06287542b899908db9b0bb2f20437f` 失效,签发新 token;
2. 新 token 只写入工作区 `home/store/home_secrets.json`(**该文件加入局部 .gitignore**,example 保留占位);
3. 记录轮换时间到 Problem-20261025.md 对应条目。

**验收**:旧 token curl 返回 401;home 插件 redtasks 拉取正常。

**代码侧移除**在 1.5 完成(本条只是止血,防止窗口期内继续泄露)。

### 0.6 moss-tts-onnx P0:分块拼接截断 【§8.3-15】

**问题**:`convert.py:400-405` 把"音频块 + 静音段"混在一个列表里取 `min_len` 再统一截断——静音段只有 ~11520 样本,导致每个音频块被截到 0.24s。

**方案**:拼接根本不需要 min 对齐(同一模型的输出声道维一致、长度维天然可变),直接顺序拼接:
```python
# 伪代码规范(不在此实现)
pieces = []
for ci, chunk in enumerate(chunks):
    pieces.append(self.decode_full_audio(self.generate_audio_frames(rows)))
    if ci < len(chunks) - 1:
        pieces.append(np.zeros((self.codec_channels, pause), dtype=np.float32))
audio = np.concatenate(pieces, axis=1)
```
若实测存在"块间声道数不一致"的历史原因(这可能就是当初写 min 的动机),则:仅对**音频块**取 min/或对齐到 codec_channels 断言失败即报 `E_INTERNAL`,静音段永不参与 min。

**涉及**:`moss-tts-onnx/convert.py:395-405`

**验收**:>75 token 文本合成,输出时长 ≈ Σ块长 + 间隔,播放正常;75 token 以内单块用例不回归。

---

## 阶段 1:安全加固

### 1.1 Electron 窗口安全基线统一 【P1-3 / §8.3-22 耦合】

**问题**:主窗(main.ts:112)、设置子窗(368)、overlay 窗(463)`nodeIntegration:true + contextIsolation:false`;仅 spawnPluginWindow(192)与 webview 收口(463-478)是对的。overlay 前端(colorpicker_overlay/overlay.html、translate_overlay/sel.html)直接 `require('electron')` 用私有通道。

**方案分四步,每步独立可验证**:

**步骤 A:建宿主 preload 桥**
- 新建 `ui/src/preload.ts`:
  ```
  contextBridge.exposeInMainWorld("octHost", {
    invoke: (ch, pl) => ipcRenderer.invoke(ch, pl),   // 通道白名单见下
    onEvent: (ch, cb) => { /* 只允许 kernel:* / overlay:* 前缀 */ },
    window: { minimize, toggleMaximize, close, isMaximized(cb) },
  });
  ```
- 白名单常量集中一处(新建 `ui/src/channels.ts`,宿主与 preload 共用):`tool:invoke`、`win:*`、`overlay:env`、`overlay:phase`、`overlay:set-capture`、`overlay:close`、`kernel:event`。`invoke/onEvent` 对不在白名单的通道直接 reject——**这一步同时把 §8.3-22 的私有通道暗耦合变成显式声明**。
- renderer.ts 的改造点:grep `require("electron")` → 全部替换为 `window.octHost.*`(已知至少 renderer.ts:274 的 `ipcRenderer.invoke("tool:invoke", ...)`)。

**步骤 B:主窗切换**
- main.ts:112 改 `nodeIntegration:false, contextIsolation:true, preload: PRELOAD_PATH`;
- 回归:侧栏/设置 11 节/命令面板/热键/托盘/剪贴板同步/文件注入。

**步骤 C:设置子窗**(main.ts:368,加载同一 index.html,改造点同 B,回归设置表单)。

**步骤 D:overlay 窗 + 插件 overlay 前端**
- 新建 `ui/src/overlay-preload.ts`:只暴露 `overlay:env/phase/set-capture/close` 与 `invoke("rpc")`(即 sel.html:104 的 `invoke('rpc','plugin.call')` 通道);
- `colorpicker_overlay/ui/overlay.html:33,50,118`、`translate_overlay/ui/sel.html:69-110,318` 的 `require('electron')` 全部替换为 `window.octOverlay.*`;
- main.ts:463 的 overlay webPreferences 切隔离 + preload。

**边界**:
- index.html 里的内联脚本若用了 `require`(渲染层历史遗留),同样替换;
- webview(browser-panel.js)已有收口逻辑不动,但确认其 guest 页无 node 残留。

**验收**:三个窗口组 `webPreferences` 全部 `nodeIntegration:false + contextIsolation:true`(grep main.ts 断言);全功能回归清单(侧栏/设置/overlay 取色/翻译选区/浏览器面板)通过;渲染进程 `typeof require === "undefined"`。

**回滚**:每步一个 commit,按窗回退。

### 1.2 UI 桥 origin 白名单(oct-bridge.js) 【P1-4 / §8.2-5】

**方案**:
1. 新建 `resources/theme/oct-bridge.js`(与 octsel.js 同分发路径,ui/package.json 的 build 拷贝步骤加这一条;插件 UI 经 `/res/theme/oct-bridge.js` 引用):
   - `octBridge.origin`:从 `document.currentScript.src` 解析,或由宿主在 iframe URL 加 `?origin=` 传入;
   - `octBridge.post(msg)`:`iframeToParent` → `parent.postMessage(msg, octBridge.knownOrigin)`,knownOrigin = 宿主创建 iframe 时写入的精确 `http://127.0.0.1:<port>`;
   - `octBridge.on(cb)`:校验 `ev.origin` 以 `http://127.0.0.1:` 开头且 `ev.source === parent`(子收父)/`ev.ports/source` 为已登记子窗(父收子);
   - `octBridge.ready()`:向父发 `oct.ready`,收到 `oct.hello`(含 token)后 resolve——**token 只发经校验的子页**。
2. 宿主侧 renderer.ts:6 处 `postMessage(..., "*")`(228-305 一带)改用同一模块;`oct.hello` 发送前校验子 iframe 的 src origin 与自身一致。
3. 逐插件替换(清单 = §8 中标注 `"*"` 的 UI):colorpicker/ui/app.js、hashcheck/ui/src/app.js、translate/ui/app.js、translate 子窗、tree/ui/app.js、proxy/app.js、mdeditor/ui/app.js、home/ui、conversion/ui、translate_overlay/sel.html。

**验收**:全仓 grep `postMessage` 无 `"*"` 参数;用 file:// 页面模拟恶意父/子,消息被拒;全插件页功能正常(hello 握手成功)。

### 1.3 gate.file_exists 权限位 【P1-5】

**方案**:`kernel/internal/lifecycle/plugin.go:436-442` 改为与 file_read 同构:
```
case "gate.file_exists":
    if ok := p.gate.Check(perms.FileRead); !ok { → 错误帧 E_PERM_DENIED }
    ...os.Stat...
```
**配套**:grep 正本插件目录哪些调 `gate.file_exists`(预期 mdeditor/browser 类),给它们的 manifest `permissions` 补 `file_read`,否则升级后功能回归。

**验收**:未授权插件负向用例返回 -32007/E_PERM_DENIED;授权插件正常。

### 1.4 /res/、/plugin/ 静态服务鉴权 + 路径穿越收口 【P1-6 / P2-7】

**问题**:`ws.go` 的 `serveRes/servePluginUI`(1009-1072 一带)无鉴权,本机任意进程可拉;前缀校验大小写敏感,Windows 可绕。

**方案(两档,推荐先甲后乙)**:
- **甲档(立即,低破坏)**:
  1. 校验请求头:放行条件 = `Origin` 或 `Referer` 以 `http://127.0.0.1:<本服务端口>` 开头,或 `Sec-Fetch-Site: same-origin/none` 组合 `Host: 127.0.0.1:*`;其余(典型 curl)401;
  2. 监听确认绑 `127.0.0.1`(app.go 起 listener 处核对一次);
  3. 前缀比较改 `strings.EqualFold` + `filepath.Clean` 归一后再比。
- **乙档(过渡一个迭代后)**:WS 增加 RPC `ui.ticket` → 单次有效 30s 的 one-time ticket;宿主创建 iframe 时以 `?t=ticket` 首跳载入,内核 Set-Cookie(绑定 loopback origin)后资源请求走 cookie,ticket 作废。

**边界**:Electron iframe 首跳会自动带 Referer/Sec-Fetch——甲档对宿主自身零改动;风险是某些隐私模式剥 Referer,故乙档作为完备解。

**验收**:无头 curl `http://127.0.0.1:<port>/plugin/<id>/index.html` → 401;`..\` 与大小写变体 → 403/404;宿主插件页加载正常。

### 1.5 凭据治理(home / proxy / 通用) 【§8.3-10/18】

**home**:
1. `redtasks.go:29-31` 删硬编码 token/base URL,改从 `store/home_secrets.json` 读(键名沿用 example),缺失时 `redtasks.state` 返回明确错误引导配置;
2. `store/home.json` 的真实 IP 移入 secrets 或 config(保留 example 脱敏版);`home_secrets.json`、真实 `home.json` 入局部 .gitignore;
3. `home.bat`:`shutdown /s /f /t 120` 计划任务改为带确认交互或参数开关(`MODE=NIGHT` 才关机);`D:\work\work.html`、Chrome 绝对路径、飞书 URL 移到 `home.json` 配置;
4. `checkin_api.go:56` secrets 落盘权限 Windows 下用 ACL 收紧(非 Windows 0600)。

**proxy**:
1. `profile_config/default.yaml.example`(0.4 已脱敏)为唯一入库版;真实 profile 存 `state/plugins/proxy/profiles/`;
2. 首启引导:app.js 检测无 profile 时显示"导入订阅/选择文件"引导(现有文件选择逻辑复用);
3. `default.yaml.example` 的 `allow-lan: false`。

**通用**:规范一条"凭据只存 `state/plugins/<id>/secrets.json`"写入 插件设计规范.md §5.2 增补;tools 检查器(5.1)加凭据模式扫描。

**验收**:`git grep` 全仓无真实 token/uuid/内网 IP;home/proxy 全功能手工回归;File > 0644 类权限抽查。

### 1.6 translate XSS 与 token 出 URL 【§8.3-21】

1. `translate/ui/sub_win.html:207-208`:`innerHTML` 拼接改 `createElement + textContent`(或复用 app.js `esc()`);同文件检查其他 innerHTML 点;
2. `result_win.html:82`、`sub_win.html:131` 的 `?kport=&auth=` 改:窗口 URL 只带 kport,token 经 `oct.hello`(1.2 桥)在 origin 校验后下发;
3. 历史残留:教用户清一次窗口历史不可行,方案上直接换通道即消除新增暴露。

**验收**:OCR 文本注入 `<img src=x onerror=alert(1)>` 字面渲染无执行;URL/历史/日志无 token。

---

## 阶段 2:协议与错误码统一

### 2.1 ErrorTable 补全 + 重生成 【P1-1 / §8.3-6 表外码】

1. `kernel/pkg/protocol/protocol.go` ErrorTable 增 `{ErrModelNotReady, DiagModelNotReady, ErrText[ErrModelNotReady]}`(-32019);
2. 决断 Python `E_MODEL_UNAVAILABLE`(§8.3-6/models.py:151):**新增** `-32020 ErrModelUnavailable` 进 ErrorTable(语义 ≠ not-ready:路径不存在 vs 未加载),protocolgen 同步生成;
3. `codes.go:55` 边界注释修正为实际范围;
4. `cd kernel && go generate ./...`;产物 diff:`errors.json`、`ui/src/protocol-errors.ts`、`sdk/node/src/protocol_errors.js`、`sdk/python/oct_sdk/protocol_errors.py`;
5. 在 5.1 的 CI 加"protocolgen 重生成 diff 必须为零"检查,防再次漂移。

**验收**:三端 grep `E_MODEL_NOT_READY`/`E_MODEL_UNAVAILABLE` 均有;sdk/python models.py 抛出的两个 diag 都能 `CODES.get` 命中。

### 2.2 抽取 sdk/go 共享包 【§8.2-1/7】

**目标**:终结 11 个 Go 插件各自手写 JSON-RPC + 错误码镜像;行为差异(非法帧/-32700、shutdown 回包、recover)一次定死。

**包设计** `sdk/go/octsdk`(独立 module,根 go.work 汇总 kernel + sdk/go + 各插件 module):
```
octsdk/
  frame.go        // Request/Response/Error 帧、WriteResponse(带写锁)
  handshake.go    // $/handshake 收发与校验
  loop.go         // 行读循环:非法帧→回 -32700;空 method→-32601;每请求 goroutine + recover + 可配并发上限
  lifecycle.go    // shutdown:先回 {ok:true} 再退;ping;busy 计数(§2.5)
  errors_gen.go   // 由 protocolgen 生成(见下),禁止手改
```
- `cmd/protocolgen` 增加 `-go-out sdk/go/octsdk/errors_gen.go` 方言输出(常量名 `CodePluginMissing` 等 + `DiagByCode` map)——错误码从此零手抄(capability-gateway 类抄错永久绝迹);
- 事件:`sendEvent(type, data)` 统一 `event.emit` 帧。

**迁移顺序**(每迁一个跑其冒烟 + commit):
tree(质量最低,当小白鼠)→ data-convert → hardware → image-convert → media-convert → hashcheck → colorpicker → capability-gateway(错误码随之修正,即 2.3)→ home → rapidocr-onnx → translate → conversion(最后,因为它还要暴露 tool.settings 代理等非标面)。

**各插件迁移要点**:删本地 codes.go/rpc 帧结构/读循环,manifest 不动;`shutdown`/非法帧行为自动对齐 SDK 规范;rapidocr/translate 的"每请求 goroutine"改用 SDK 的并发上限参数。

**验收**:正本插件目录 grep 无重复 `codePluginMissing =` 定义;`go build ./...`(经 go.work)全绿;e2e-conv-all + smoke 回归;`tree` 迁移后行为对比(非法帧现在回 -32700,记录为预期变化)。

**回滚**:逐插件 commit,单插件回退不影响他人。

### 2.3 capability-gateway 错误码与 status 【§8.3-2】

1. 随 2.2 迁移后自动修正(main.go:43-45 手抄值被生成常量替换);
2. `status`(main.go:678-679 的空 stub)二选一:真实现(聚合 `gate.models_list` + 偏好快照)或从 manifest 删声明——**建议真实现**,该函数是 UI 诊断入口;
3. `call()` 保留并透传 `error.data.diag`;`isVRAMError` 改判 diag 而非子串匹配(main.go:120-131,553-559);
4. invoke 前 `gate.settings_get` 加内存缓存(失效条件:settings.set 事件)。

### 2.4 双 SDK 对齐 【P1-2】

| 项 | Python(sdk/python/oct_sdk) | Node(sdk/node/src) |
|---|---|---|
| 默认超时 | `__init__.py:101-111` `_post` 的 `q.get(timeout=30)`,超时抛 `ProtocolError("E_TIMEOUT")` | 已有 30s,超时错误改带 diag |
| 语义错误 | 已有 ProtocolError | **新增** `ProtocolError` 类(code/diag/message),`post` reject 时携带 |
| 模型加载权 | models.py 已有 | **新增** models.js(ensure/release/acquire,对齐 models.py 语义含 E_MODEL_NOT_READY) |
| busy | 已有 §10.5 上下文 | **新增** `sdk.busy(count)`/`busy(0)` 帧封装 |

**契约测试**(新增,防再次分叉):`sdk/node/test/contract.mjs` 与 `sdk/python/tests/test_contract.py`,用同一份用例表(假内核 stdin/stdout 脚本)断言:握手、超时、错误帧解析、busy 三端行为一致。

**验收**:双端契约测试绿;Python 插件在内核不响应场景 30s 报错而非挂死。

### 2.5 busy/limits 落地 【§8.2-3】

逐插件包裹点(任务开始 `busy(1, maxBusyMs)` / 结束 `busy(0)` 或引用计数):
| 插件 | 位置 |
|---|---|
| conversion | convert.go:37-52 编排层总入口 |
| media-convert | convert.go:167-169(600s) |
| doc-convert | convert.py 转换入口(180s) |
| moss / opus | run_tts/translate 入口 |
| translate | 语音循环与单次翻译 |
| mdeditor | write 大文件(配合 4 波的帧上限改造) |
| tree | scan 入口 |
| hashcheck | 改引用计数(task start/end 配对,任务表清空时归零) |
| capability-gateway | invoke 总入口 |

manifest `limits` 同步补声明(media-convert 的 request_timeout_ms 模式推广)。

**验收**:长任务期内核状态 busy;人为 kill 任务不再触发 watchdog 误判;hashcheck 并发双任务结束时 busy 不提前归零。

---

## 阶段 3:内核正确性

### 3.1 状态机竞态收敛 【P1-7 / 旧P1-13】

1. **deps 后台安装结果入事件流**:`manager.go:432-440` 的 `depsInstallThenStart` 不再直接写 `r.depsPending/r.state`;定义 `stEvent{op: opDepsDone | opDepsFail, id, err}` 投 `stEvCh`,stateLoop 消费后落状态(与 1048 行 RegIdle 的合规路径合流);
2. **mf.Entry 写加锁**:`manager.go:1032` 写 `r.mf.Entry` 持 `r.mu`(或局部构造后锁内替换);
3. **exitCode**:`plugin.go:321` 改 `atomic.Int32`(readLoop 写 / ExitCode 读);同文件 44 行注释同步更新;
4. 新增竞态测试:`TestDepsInstallConcurrentWithList`(并发触发 deps 安装与 Describe/All)、`TestPluginExitCodeRace`;CI 固化 `go test -race ./...`。

**验收**:`go test -race ./internal/...` 绿;deps.install 期间插件列表请求无 panic/脏读。

### 3.2 事件顺序化 + rpc 监听器治理 【P2-1 / 旧P2-30/40/65】

1. `emitState`(manager.go:263-270)改为 stateLoop 在状态落定后**同步顺序**调用广播(广播内部已有 ws 写锁,不会阻塞状态机——若担心慢消费者,引入带序号的 outbound 队列,单 goroutine 排空,保序);
2. `main.ts:951-975` rpc 监听器泄漏:改**单监听器分发**——`ws.on("message")` 一次注册,维护 `pending: Map<id, {resolve,reject,timer}>`,按帧 id 路由;`rpc()` 只登记 pending;
3. renderer `pollState`(renderer.ts:83-92)从 600ms×8 降为 30s 一致性校验 + `kernel:event` 驱动刷新(事件序已可靠,轮询只是兜底)。

**验收**:快速 start/stop 循环 20 次,侧栏状态序列单调无跳帧;长会话下 `ws.listenerCount("message") === 1`。

### 3.3 runtime.Installer 假实现决断 【P0-2】

**推荐方案 a:真实现**(package.ps1 venv 三态与"首启自动安装"承诺依赖它):
1. `Install()`:解析 venv 路径 `<state>/plugins/<id>/venv` → 定位 uv(BundledPythonDir/env/PATH)→ `uv venv --python <manifest.python_version>` → `uv pip install -r requirements.txt`(镜像源用 config 已有的 mirror 设置)→ 写 marker `.oct-installed{hash, ts}`;
2. `onPhase` 映射:resolve-python / create-venv / install-deps / done,经 ws 的 `deps.install` 进度事件广播(UI 已有进度条消费);
3. `Ready()` = marker 存在且 hash == DepsPlan().lockHash;hash 不匹配 → 触发重装;
4. `lockHash` = sha256(依赖文件内容)(与 0.1 的 GenerateLock 对齐);
5. `ListPackages/NodePackages` 走 `uv pip list --format json` / `npm ls --json`(在插件 venv/node_modules 内执行);
6. Node 依赖链:`npm ci --prefix <plugin>`(package-lock 存在时)否则 `npm install --omit=dev`;
7. 失败语义:任何 phase 失败 → `E_DEPS_INSTALL_FAIL` + phase 名,registry 状态 depsState=error,UI 可重试;中断幂等(重跑跳过已完成 phase)。
8. 同步修正文案与实现一致:manager.go:192-199、app.go:81 注释如实现不符则更新(现在是反向——实现追上注释)。

**备选方案 b(若 2 周内无人力做 a)**:诚实降级——删 renderer.ts:1047-1058 的"安装并重启"按钮与"需数分钟"提示(改为"依赖未就绪,请手动执行 tools/install-deps.ps1");package.ps1 头部与 §6 相关注释改为"依赖需手动预装";`Install()` 回调文案改 `"manual mode"`。

**验收(方案 a)**:删除一个 Python 插件 venv → UI 点安装 → 进度事件可见 → 完成后插件自启;模拟 uv 不存在 → E_DEPS_INSTALL_FAIL 且 phase 明确;二次安装幂等(秒回)。

### 3.4 资源/看门狗假成功与死配置决断 【P2-2/3】

| 项 | 位置 | 决断 | 做法 |
|---|---|---|---|
| tool Acquire 假成功 | resources/tool.go:39-46 | **实现** | spawn 钩子检查 `Locate().Found/Err`;未找到 → Acquire 失败 `E_TOOL_UNAVAILABLE`;`wellKnownPaths`/注册表探测从此有意义 |
| LastActive() 恒 now | resources/map.go:447 | **实现** | 记录最近 Acquire/使用时间;供回收策略消费(若无消费者,保留实现+单测,标注"备用于回收") |
| Watchdog.Run 无调用 | watchdog.go:42-64 | **删除** | manager 用 patrol+Tick 已覆盖;删 Run 或给 Run 加调用方(二选一,倾向删) |
| profile 联网开关只校验不执行 | contract.go:25 effectiveNet | **实现** | 落地执行:spawn 时按 profile 注入环境(net:false → 注入 `OCT_NET=off`,并要求外部进程包装器拦截;短期可先做"转换类插件禁 URL 输入"的白名单校验);**此项同时消解 media-convert `net:false` 纸面问题(§8.3-14)与 conversion/network 权限矛盾** |
| CPUPercent | lifecycle.go:109-118 | **删除 UI+字段**或实现 | 实现=Job Object `SetInformation(JobObjectCPURateControlInformation)`;一期删除 UI 项,字段标注 reserved |
| Heartbeat.IntervalMs | lifecycle.go:69-74 | **实现** | healthSweep ticker 改读配置(默认 1s);5 分钟工作量,建议实现 |
| PrewarmPool/KeepAlive | lifecycle.go:61-66 | **删除** | Override/UI 表单同步删(旧P2-49) |
| Triggers/HasTriggers | lifecycle.go:120-139 | **移阶段 6** | 单独立项(懒启动触发器是真需求);先在规范标注 P2 |
| 模型 unloading 卡死 | resources/map.go:413-415 | **实现** | Release 归零后转 `registered`;自锁路径加 backend==self 防御 |
| perms.Subset 排序 | perms.go:174-187 | **实现** | 返回前 sort.Strings(旧P1-10) |

**验收**:未装 LibreOffice 时 conversion 返回 `E_TOOL_UNAVAILABLE`(而非转换失败);`go test ./internal/...` 含新增 tool.locate 用例;UI 不再展示无效配置项。

### 3.5 旧报告 P1/P2 小修批量清账

| 旧编号 | 修法 | 位置 |
|---|---|---|
| P1-6 | build.ps1:29 / start.bat:27 文案改读 go.mod(脚本内 `go env`/正则解析) | build.ps1、start.bat |
| P1-7 | 设置节 names A~L 补全,并改为从单一 `sections.ts` 派生(main.ts:215 与 renderer.ts:27 共用;index.html `data-sec` 同步)——为 6.2 铺路 | ui/src |
| P1-8 | 删 index.html:478-514 `#panel-debug` 整块(重复 id 死面板) | ui/index.html |
| P1-9 | 删 renderer.ts:78,89 `RUNNING_EXT` 分支 | ui/src/renderer.ts |
| P1-12 | registry.go:252 注释改为如实描述(不实现 DisallowUnknownFields,避免旧 registry 拒载) | registry.go |
| P1-14 | ws.go:716-728、523,541 补 `s.installer==nil`/`s.gate==nil` 判空,风格统一 | ws.go |
| P1-15 | settingsSchema 过滤收敛:`ws.go:394-405` 改调 `config.SchemaProps` 单实现,边界行为以"schema 为空不过滤"为准并加测试 | ws.go、config.go |
| P2-20/21/22/23 | 注释乱码修;`ws.go:198` 错误帧 id 用 `req.ID`;default 分支加日志;plugin.go:218 错误分类改 `E_STDOUT_CONTAMINATED` | ws.go、plugin.go |
| P2-36/38 | renderer.ts:806 与 index.html:263 的 `../resources/...` 改运行时 `resBase` 注入(加载前替换 src),打包模式 404 消除 | renderer.ts、index.html |
| P2-39 | main.ts:1141 auth 解析失败路径 `kernelProc.kill()` 后再 reject | main.ts |
| P2-43 | main.ts:58-62 tool:invoke 超时改读目标插件 manifest 的 request_timeout_ms(经 plugin.details 透传) | main.ts |
| P2-44 | plugin.go:428 AfterFunc Kill 与 KillTree 双杀收敛:KillTree 前取消 AfterFunc | plugin.go |
| P2-57 | download.go:97-99 并发 PullModel:按 modelID 加 in-flight map 去重(等待者共享结果);断点续传标阶段 6 | download.go |
| P2-64 | 死 API 批量处理:ndjson.go/Marshal/IsHighRisk/discover 等逐个"删或接线";FR-10 高危二次确认(IsHighRisk)建议接线到宿主权限弹窗 | 各处 |
| 旧P2-58 | sdk/python `home_dir()` 兜底改报错而非错误路径 | sdk/python |

**验收**:Problem-20261025.md 三/四节逐条标 `[已修]`;全量 `go test ./... && go vet ./...` 绿。

---

## 阶段 4:插件层三波修复

> 完整问题定义见 Problem-20261025 §8.3。此处给"修什么、怎么修"卡片;执行时按波分组 commit。

### 第一波(随阶段 0)
0.6 moss / 0.5 home token / 0.2 palette(宿主侧)。

### 第二波:换机可用性(路径 + 权限 + 依赖声明)

**4.2.1 模型路径三层来源**(统一改法,四个插件):
- 优先级:宿主 `model.ensure` 下发(已支持)→ manifest `providesModels.path` 支持 `${OCT_MODELS_ROOT}` 展开 → 报 `E_MODEL_UNAVAILABLE`(2.1 后该码已存在);
- hy-mt:manifest.json:33 + convert.py:21;moss:manifest.json:45,58 + convert.py:32-33;opus:manifest.json:33,47 + convert.py:25-26;rapidocr:main.go:67-68 + manifest providesModels;
- `probe()/status` 的就绪判定同步改为"读宿主 models.acquire 状态或展开后路径",不再绑死默认值(hy-mt convert.py:170、sherpa convert.py:329、rapidocr main.go:252)。

**4.2.2 rapidocr ORT DLL**:
- ort.go:106 删 `D:\LanguageTool\...` 候选;nativeLibs `sha256` 必填(随包分发 onnxruntime.dll 并算哈希)或 `optional:false` + PATH 兜底**必须**校验签名(阶段实现:对 PATH 命中的 dll 计算哈希与 manifest 比对,不符拒载)。

**4.2.3 权限对齐**(manifest 改动 + 内核 perms 无需变):
| 插件 | 改动 |
|---|---|
| hy-mt / sherpa | 删 `execute_command`、`spawn_process` |
| doc-convert | 二者取一(留 `spawn_process`,pandoc/soffice 外调属进程派生) |
| proxy | 补 `spawn_process` |
| napi_rs_tool | 权限改如实际能力:load 限定只接受 `<OCT_PLUGIN_HOME>/../colorpicker/native/...` 相对白名单(按 manifest `dependencies` 声明解析,替代任意绝对路径 require),并在 DESIGN.md 更新威胁模型 |
| conversion / media-convert | `network` 权限与 profile net 开关对齐(3.4 effectiveNet 落地后:net:false profile 下权限不申请) |
| colorpicker | `permissions:["state"]` → 删(或换成实际所需,历史持久化走 OCT_PLUGIN_DATA 后无需权限) |

**4.2.4 browser SDK 路径**:
- `process/env.go` 注入 `OCT_SDK_NODE=<repo>/sdk/node/src`(spawn 时由内核计算);browser/main.js:6 改 `require(process.env.OCT_SDK_NODE || path.join(...))`;
- 同时删 main.js:14-18 私人飞书 URL(默认页改空+示例格式),`load_mode` 改 `lazy`(manifest:8)。

**4.2.5 其余硬编码**:home.bat(见 1.5)、`netStaticIP`(net_windows.go:12 → home.json 配置)、e2e-diag.mjs:11-13 的 D:\ 路径改参数/argv、ui/index.html:313 placeholder 改相对路径示例(旧P1-18)。

**验收**:临时改 `OCT_MODELS_ROOT` 指向副本目录,四个推理插件全部可用;`git grep -n "D:\\\\" -- mvp_test/plugins kernel ui tools` 逐条复核仅剩注释;未授权插件调 file_exists 被拒(1.3 联动)。

### 第三波:功能正确性(逐插件卡片)

**browser**:startup.list/save 补进 manifest functions;`OCT_PLUGIN_DATA` 缺失时报错不落安装目录;ui/app.js:63 补 ws 重连(监听 close → 指数退避重连 → 重发 hello)。

**capability-gateway**:见 2.3;另修 setPreference 支持显式清空(传 null = 回退默认)、strategy/providerChain 枚举校验;降级链加总预算(timeout 总和上限,超出直接失败)。

**colorpicker**:状态落 `OCT_PLUGIN_DATA`(main.go:25-33);saveState 错误上报(main.go:69-74);非 JSON 帧回 -32700(随 sdk-go 迁移自动修);ui/app.js:113,197 假成功改真实反馈;`../colorpicker/ui/build/colorpicker.node` 相对路径改经 manifest dependencies 解析;Rust 侧:GDI 失败路径释放 memdc/hdc(lib.rs:163,388)、抓屏失败返回错误而非 #000000(lib.rs:199-202);S=21 常量单源(manifest 或生成头文件,阶段 6 处理可接受)。

**conversion**:busy(2.5);scanManifests 加 mtime 缓存(无效化:registry 变更事件);saveRoutes 锁内快照一致(route.go:69-94);UI 删"自动下载官方版"假选项(app.ts:904-908,947-951)或 loInstall 实现真下载(建议删,诚实);app.ts/app.js 双版本问题:index.html 改引用 src/app.js 的构建产物路径并加"产物过期检测"(build 时写 hash 注释,check-all 校验 ts 新于 js 则失败);`conversion.exe~` 删。

**data-convert**:emitYAML 失败改抛 `E_INTERNAL`(yaml.go:78-85);.xls 写入透传 `E_TOOL_UNAVAILABLE`(convert.go:114 + xlsx.go:43-47);输出 tmp+rename;shutdown 由 sdk-go 规范接管;删手写插入排序、双层 goroutine 嵌套;输入大小上限(如 50MB,超限报错);go.mod 版本对齐(工具链统一 1.26,随 sdk-go)。

**doc-convert**:权限取一;yml→yaml 别名改映射到真实 pandoc writer(或删该别名);postprocess 默认值改"显式开启"(配置里 `postprocess.default=false`,setConfig 过一次不再隐式全局生效);pdf→doc 中间文件改 `%TEMP%` 生成后 move;`__pycache__` 清理(0.4 已做);自建 settings 通道迁 `gate.settings_get/set`(settingsSchema 已声明,重复实现删除)。

**hardware**:删无扩展名产物(0.4);manifest 补 `kind:"app"`;CPU/PDH 采样锁粒度缩小(采集在锁外、快照在锁内)或采样结果缓存 1s;事件日志缓存 key 去掉 max 维度(固定 200,UI 分页);ui 补 `ws.onclose` 停轮询 + beforeunload;sensors 占位从代码删(或实现)。

**hashcheck**:busy 引用计数(2.5);任务表上限策略改"超限时拒绝新任务并报错"(比淘汰更诚实);`os.ExpandEnv` 保留但文档注明 + 只对显式 `expand:true` 参数生效(避免路径含 % 意外展开);verify 批量清单列为 P1 待办进 README。

**home**:1.5 全部 + 读循环异步化(net.status 里的 PowerShell/ipify 移入 goroutine + 结果缓存 30s,`probeNet` 间隔 8s→60s 或事件驱动);非法帧 -32700(sdk-go);needClock 竞争加锁;500ms tick 的 readFlags 缓存+mtime 判断;`trimSpace/itoa/var _` 死代码删;UI 脚本外移 app.js + `catch(e){}` 改提示。

**hy-mt-server**:4.2.1 路径;`UnicodeDecodeError` 捕获归 E_PARSE;删 convert.py:75-77 死 loader;逐行推理加单行超时(30s)与 max_tokens 提示(start.lock 随 0.4 忽略)。

**image-convert**:解码/编码失败改 codeIO/diagIO(main.go:131,140);`json.Unmarshal` 错误回 -32602;ffmpeg/resvg 加 CommandContext 超时(image.go:130-141,185-195,默认 120s);manifest functions 补 state/status;`dependencies:[]` 改如实声明(oksvg/rasterx)或 vendor;build.bat 文案改;SVG 回退检查 Draw 错误返回(或至少 UI 提示降级);并发上限 2(ffmpeg)。

**mdeditor**:scanner 上限分级——write 通道改独立大上限(≥10MB)或分片写(推荐:write 参数加 `chunks` 协议,或一次性放宽 write 行上限并在 read 侧维持 1MB);read/write 扩展名白名单(`.md/.markdown/.txt` + manifest `filePatterns` 声明);setroot 记录进 state 并在 UI 明示当前根;beforeunload 保存改 `navigator.sendBeacon` 风格(经主进程同步 IPC flush,窗口 close 前阻塞确认);KaTeX 定界符修正 `'\('`;monaco 瘦身进阶段 6;`os.Exit`/非法帧随 sdk-go;`mdeditor_state.json` 落 OCT_PLUGIN_DATA。

**media-convert**:profile net 门控(3.4 落地后验证 m3u8 URL 被拒);ffmpeg 失败改 codeIO;预检输入存在/输出目录;并发上限 + 同输出路径去重(第二次请求复用任务句柄);manifest 补 state/status;`network` 权限对齐。

**moss**:0.6 已修 P0;补 busy;`speed`/`params.model` 实现或删注释(建议实现 speed:重采样系数;model 覆盖并入 4.2.1);loader 双实现收敛到 main.py 版;`maxChunkTokens` 校验(1..2048)。

**napi_rs_tool**:缓存键改路径索引:`modules: Map<absPath, entry>` + handle→abs 反查(main.js:38 一并修 has 判断);权限白名单(4.2.3);stdout 防护:加载子模块前 `console.log = ...` 重定向到 stderr(或 spawn 包装不可行,文档声明约束 + 在 DESIGN.md 记录);subscribe 加 unsubscribe(topic);unload 语义改诚实:返回 `{ok:true, note:"cache evicted, dll remains until process exit"}`;manifest functions 补五个方法;DESIGN.md on_demand/lazy 口径修正。

**opus-mt**:4.2.1 路径;删 `_ZH_RANGE`;translate_batch 改真 batch(逐行收集 source,按 8 行一批);`max_decoding_length` 设 512 + 超长检测告警;spm 定位改 probe 检查 `source.spm/target.spm` 存在;输出 tmp+replace;busy。

**proxy**:1.5 凭据 + allow-lan:false;`freeControllerPort` 占用后直接传给 mihomo(不经释放-重绑,消除 TOCTOU;mihomo 支持 external-controller 指定端口);UI 轮询 document.visibilitychange 暂停;escapeText 删或实现;Kill 错误日志补;`stopCoreLocked` 失败上抛事件。

**rapidocr**:引擎生命周期:currentEngine 取指针与 recognize 之间加 RLock(`engMu.RLock` 持有期间 releaseEngine 的 Lock 进不来),release 后置 nil 由 RLock 释放后再 close——具体:`handleOcr` 在 RLock 内完成 recognize,release 在 Lock 内 close;4.2.1/4.2.2 路径与 DLL;ctcDecode 的 conf 累加修复并真正返回置信度;timeoutMs 接 context;status 判定改 4.2.1;边界(1px/0 尺寸图)守卫;functions 补声明;并发上限 1(引擎已互斥,排队上限防积压)。

**sherpa**:params.model 贯通(loader 闭包改接受 model_dir,修真实 bug convert.py:231,234);权限删(4.2.3);DEFAULT_SPEAKER_ID 改按名单 `index("zf_xiaobei")` 动态求;setConfig schema 校验 + speed try/except;numpy 进 requirements.txt;死 loader 删;start.lock 清(0.4)。

**translate**:voice.mu 锁内 RPC 移出(先锁内取快照→解锁→RPC→锁内写回);voiceLoop/subS.walk 补锁;ffmpeg -list_devices 加 3s 超时;micRecorder 启动后 1s 验活,失败即报错事件;assertPerm 区分错误类型(仅 -32007 映射未授权,超时/方法缺失原样透传);capture_other.go 补齐三个函数桩或统一加 `//go:build windows` 并在 manifest/README 声明 Windows-only;XSS/token(1.6 已修);`config.set` 改合并写(读-改-写,保 region 键);modelsStatus 去重逻辑修;`%TEMP%` 文件桥改内存传递(调 opus/hy 的 convert 方法本可直接传文本,经文件是历史遗留——改 RPC 参数直传);busy;manifest 补 kind + 11 方法。

**translate_overlay**:通道迁移(1.1 步骤 D 后,sel.html 的 ipcRenderer 全走 octOverlay);`pluginId:'translate'` 改 manifest `dependencies:["translate"]` 声明 + 启动时校验,缺失时报错提示而非静默;main.js 分发真实方法(空壳只回 ok → 改回 `$.noop` 语义:methods 为空即合法,但对未知带 id 请求回 -32601);catch(()=>{}) 改 console.error + UI toast;envResolve 双 resolve 修(clearTimeout)。

**tree**:sdk-go 迁移(错误码/-32700/shutdown 全套随之修);load_mode 改 lazy;manifest 补 kind、删空 commands;UI origin 校验(oct-bridge);callPlugin 防御 result 缺失。

**全插件通用收尾**:局部 .gitignore 就位后 `git status` 复核;`tools/check-manifest.mjs`(见 5.1)跑通并修全部"实现了未声明/声明了未实现";oct-bridge 替换后逐插件 hello 冒烟。

**验收**:对照 §8.3 逐插件勾稽表:P1 清零、P2 清理率 ≥80%(剩余项在 Problem 文档标注"接受/延后");e2e-conv-all 全格式矩阵通过;三个 overlay 场景(取色/翻译选区/编辑器子窗)手工回归。

---

## 阶段 5:工程化

### 5.1 检查器与 CI

**新增 `tools/check-all.mjs`**(本地一键)聚合四类,CI(本地先以 pre-commit 钩子或手动跑,远端 workflow 后补)同一入口:
1. **构建与测试**:`go build ./... && go vet ./... && go test -race ./...`(经 go.work 覆盖 kernel + sdk/go + 插件);
2. **protocolgen 一致性**:`go generate ./...` 后 `git diff --exit-code`(覆盖 errors.json + 三端产物 + sdk/go 生成物);
3. **manifest 对账**(新工具 `tools/check-manifest.mjs`):解析每个插件 manifest `functions[].method` ↔ 源码方法分发表(Go:grep `case "x.y"` / switch;JS/Python 同理)双向 diff,允许 `INFRA_METHODS = [ping, shutdown, state, status, models, model.ensure, model.release, event.*]` 白名单;同时校验 `kind` 必填、`permissions` ⊆ 内核权限位表(errors.json 旁输出)、`python_version` 与 `codeDeps.runtime` 一致;
4. **卫生断言**:`git ls-files` 匹配 `\.exe$|\.exe~$|__pycache__|\.onnx$|\.gguf$|native/target/` 为空(白名单:`kernel/kerneld.exe`、`mvp_test/plugins/proxy/resources/mihomo.exe` 若保留);凭据模式 grep(32 位 hex token、`uuid:`、`password:`)命中即失败(example 文件豁免)。

**CI runner**:先 GitHub Actions windows-latest(go + node 矩阵);若仓库不打算上 GitHub,则以 `start.bat` 前置本地钩子 + 每日手动跑代替,但**必须在某处自动化**,否则旧报告"写完不修"的循环会复现。

### 5.2 Lint / 类型 / 提交规范
- golangci-lint:`errcheck, govet, staticcheck` 起步,`kernel/` 先行,插件随 sdk-go 迁移纳入;
- eslint(`@typescript-eslint/recommended`)+ tsconfig 渐进 strict:新文件 strict,存量按文件迁移(renderer/main 最后,阶段 6 一起);
- 提交规范:conventional commits + 问题编号引用,示例:`fix(kernel): close P1-7 state race in depsInstallThenStart`;`fix(plugins): §8.3-15 moss chunk concat`。git hook(commit-msg)校验格式。

### 5.3 仓库卫生收尾
- `.gitignore` 补 `.uv-cache/`、`.trae/`(文档移 `docs/history/` 或直接忽略)、`.workbuddy-ai/`、`outputs/`;
- `kernel/kerneld.exe.bak-*` 等历史对象:确认单人仓库后 `git filter-repo --path kernel/kerneld.exe.bak-20261013-152326 --invert-paths ...` 逐 path 清除,备份 `.git` 后执行,远端强推;
- `_state.json`/`_home.html` 的历史对象同法清理(内容无关但含第三方页面)。

---

## 阶段 6:架构收敛(切片执行)

### 6.1 UI 拆分
- `renderer.ts`(1554 行)→ `ui/src/renderer/` 下:`main.ts`(装配)、`sidebar.ts`、`palette.ts`、`settings/{lifecycle,order,deps,hotkeys,resources,internal}.ts`(每节一模块)、`bridge.ts`(oct 桥与事件转发)、`inject.ts`(文件注入重试);
- `main.ts`(1510 行)→ `ui/src/main/` 下:`index.ts`、`kernel.ts`(spawn/auth/ws/rpc 单监听器)、`hotkeys.ts`、`tray.ts`、`overlay.ts`、`clipboard-sync.ts`、`file-assoc.ts`、`settings-window.ts`;
- 约束:每文件 <500 行;拆分不改行为(拆完跑全回归清单);与 1.1 的 preload/channels 模块共用。

### 6.2 设置单源
- 新建 `ui/src/sections.ts`:`export const SECTIONS = [{id:"A", name:"通用", anchor:"sec-general"}, ...]`(A~L 唯一真源);
- main.ts 子窗标题、renderer 导航、index.html `data-sec` 由脚本(或运行时)从该文件派生;CI 加"三处派生一致"检查。新增设置项 = 改 sections.ts + 一个模块文件(改动文件数 ≤2,对齐规范 §17.2)。

### 6.3 文档-实现校验
- `tools/check-doc-refs.mjs`:提取代码注释中 `§\d+(\.\d+)*` 引用 → 与 理想架构.md/插件设计规范.md 的标题锚点比对,悬空引用列清单(不阻断,先产出报告供清理);
- 配合 3.4 的死配置决断,把"文档说做了"的章节逐个改写为实际状态。

### 6.4 体积优化
- mdeditor monaco 定制构建(esbuild:core + markdown 语言 + basic-linter,目标 ≤6MB vs 现在 25MB);
- proxy mihomo.exe(59MB)评估按需下载(首启下载到 state/)+ 校验哈希,或换 mihomo-mini 分发;
- colorpicker native 构建产物(name 加载的 .node)确认走白名单路径(4.2.3)而非整 target 入库(0.4 已挡)。

---

## 附 A:问题编号 → 方案章节对照

| 问题 | 章节 | 问题 | 章节 |
|---|---|---|---|
| P0-1 runtime 编译 | 0.1 | P1-1 错误码表 | 2.1 |
| P0-2 假安装链 | 3.3 | P1-2 双 SDK 分叉 | 2.4 |
| P0-3 插件无版本控制 | 0.4 | P1-3/4 窗口与桥安全 | 1.1/1.2 |
| P0-4 git 二进制污染 | 0.3/5.3 | P1-5 file_exists | 1.3 |
| P0-5 palette 崩溃 | 0.2 | P1-6 静态服务鉴权 | 1.4 |
| P1-7 状态机竞态 | 3.1 | P2-1 事件乱序 | 3.2 |
| P2-2/3 假成功/死配置 | 3.4 | P2-4 巨型 UI | 6.1 |
| P2-5~7 | 4 波次/1.4 | P3-1~5 | 5.1/5.2/6.2/6.3/5.3 |
| §8.3 各插件 | 阶段 4 卡片 | §8.2 横向 | 2.2/2.5/4.2/1.2 |

## 附 B:总验收与节奏

- **每阶段**:跑该阶段验收 + 全量回归(`go test -race ./...`、e2e-smoke、e2e-conv-all、宿主手工清单);Problem-20261025.md 对应条目标 `[已修 <commit>]`;
- **每波插件修复**:单独分支/commit 前缀 `fix(plugins/<id>):`,失败只回退该插件;
- **里程碑**:M1=阶段 0(可构建可分发)→ M2=阶段 1(安全底线)→ M3=阶段 2+3(协议与内核正确)→ M4=阶段 4(插件清账)→ M5=阶段 5(不再回归)→ 阶段 6 按迭代;
- **总量复核**:0-5 合计约 19-23 人日(0.4 的清理与 4 波工作量有重叠,已按并算扣减)。
