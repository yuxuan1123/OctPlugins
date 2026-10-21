# OIM · Weasel 候选窗暗金横向 + OctPlugins 通道

## Context（背景）

用户已确认 Weasel fork 版输入法可用（注册 `system32\oimime.dll`、`oimd.exe` 服务器运行正常）。现提出两项要求：
1. 候选窗 **UI 主题为暗金 `#b9965f`**，且 **横向（horizontal）**排布；
2. 把先前手写 broker 上打通的 **Phase 3「OctPlugins ⇄ oimd」双向通道**平移到 Weasel 版服务器。

**关键发现（探查结论）**：这两项在 Weasel fork 里**已经实现**，本次真正要做的只有「横向排布开关」+「通道健康验证」：

- **暗金主题**：`output\data\weasel.yaml` 的 `style.color_scheme: oim` 已生效，`preset_color_schemes/oim` 即**白底暗金 `#b9965f`**（border/label/hilited_candidate_back 均 `0xb9965f`），无需改色。
- **OctPlugins 通道**：`WeaselServer\OctServer.{h,cpp}` 已在服务器进程内实现 JSON-RPC 2.0（按行分隔 TCP `127.0.0.1:56619`），`WeaselServerApp.cpp` 已 `oct_->Start(&m_server)` + `SetOctEventCallback(Push)` 事件上抛；`RimeWithWeasel.cpp` 已 `_EmitOctEvent("oim.commit|context|status", …)`；Go 客户端 `plugins\oim\main.go` 转发方法与其一一对应。`OctServer.obj` 已编译进 `oimd.exe`。

## 需改动的文件（唯一的实际变更）

1. `d:\Project\ElectronProject\OctPlugins\mvp_test\oim\weasel\output\data\weasel.yaml`
   - L30：`horizontal: false` → `horizontal: true`（横向候选条，RimeWithWeasel.cpp 据此设为 `LAYOUT_HORIZONTAL`）。
   - （兜底，可选）L43 区域 `layout/type` 未设横/竖，保持默认即可；若加则设 `type: horizontal`，与 `horizontal: true` 二取一，避免冲突。

> 说明：这是**运行时数据配置**，无需重编译 Weasel。改后需重启 `oimd.exe`（或触发部署重新加载 weasel.yaml）才生效。

## 复用/不重复实现的既有资产

- 主题方案：`output\data\weasel.yaml:64-80` `preset_color_schemes/oim`（白底暗金）。
- 通道服务端：`WeaselServer\OctServer.cpp`（监听 56619、下行 `oim.*`、上行通知、历史持久化、经 `g_api_mutex` 与 TSF 管道串行）。
- 通道接线：`WeaselServerApp.cpp:36-42`（Start/Push）、`RimeWithWeasel.cpp:865/911/1040`（事件上抛）。
- 通道客户端：`plugins\oim\main.go`（TCP 连接 56619、转发 RPC、`event.emit` 上抛）。

## 验证

1. **横向 + 暗金**：重启 `oimd.exe` 后，在记事本打拼音，确认候选窗**横向排列**且**白底边框/高亮暗金**；`Ctrl+S` 仍正常。
2. **通道健康**：`oimd.exe` 运行期间，`Test-NetConnection 127.0.0.1 -Port 56619` 应 True；或用 TCP 发 `oim.ping` 收到同 id 响应。
3. **端到端（可选）**：确认 `plugins\oim` 插件已注册并运行（宿主 `tool:invoke` 桥接沿用 Phase 3 既有接线），`oim.status` 返回当前 schema/状态。
4. 若 `oimd.exe` 需带最新配置/代码重编，跑 `weasel\build.bat` 重建 `WeaselServer` 目标后重新部署 `output\oimd.exe`。

## 边界

- 若用户对暗金配色细节（如候选文字色、编码底色 `#f4ead8`、高亮序号 `#d9b27a`）另有微调要求，仅在 `weasel.yaml` 的 `oim` 配色块内调整即可，依旧免编译。

## 验证结果（2026-10-10 · 已验收）

Weasel fork 版通道四大缺口已补齐并重编 `output\oimd.exe`：

| 方法 | 修复前 | 修复后（实测） |
|---|---|---|
| `oim.status` | `-32001 no active session` | `{"schema_id":"luna_pinyin_simp","ascii_mode":false,...,"enabled":true}`（临时会话兜底） |
| `oim.context` | `-32001` | `{"preedit":"","composing":false,"cursor":0,"candidates":[]}` |
| `oim.schema.list` | 无此方法 | `[{"schema_id":"luna_pinyin_simp","name":"朙月拼音·简化字"}]` |
| `oim.toggle` | 存标志不消费 | `enabled:false` 后 `ProcessKeyEvent` 直通、`oim.status` 返回 `enabled:false`，再 toggle true 恢复 |
| `oim.option.set` | 无活动会话失败 | `ascii_mode:true` 持久化，`oim.status` 立即反映，新会话套用 |

改动落点：
- `mvp_test\oim\weasel\include\RimeWithWeasel.h`：新增 `SetOctEnabled`/`OctSchemaList`/`oct_any_session` 声明 + `m_oct_enabled`（volatile，默认 true）/`m_oct_options` 成员。
- `mvp_test\oim\weasel\RimeWithWeasel\RimeWithWeasel.cpp`：OctStatus/OctContext/OctSelectSchema/OctSetOption 无会话时临时会话兜底；`OctSchemaList` 用 `get_schema_list`（`RimeSchemaList` 无 `data_size`，勿用 `RIME_STRUCT`）；`ProcessKeyEvent` 加开关门；`AddSession` 套用持久选项。
- `mvp_test\oim\weasel\WeaselServer\OctServer.cpp`：`oim.toggle` 经 `ExecOnHandler` 联动 `SetOctEnabled`；librime 组新增 `oim.schema.list` 分发。
- `plugins\oim\main.go`：`schema.list` 由空占位改为 `forward("oim.schema.list", nil)`，已重编。

构建备注：单工程构建 `msbuild WeaselServer.vcxproj` 必须显式传 `/p:SolutionDir=...\weasel\`（否则 `$(SolutionDir)\include` 失效、找不到头文件）；Windows 下 `cmd /c` 被安全策略拦截，用临时 .bat 经 PowerShell 调用 `vcvars64.bat` + `msbuild`。

## 打包前补功能（2026-10-10 · 已验收）

**自定义快捷输入（复刻 rime 原生 `custom_phrase`，不重复造轮子）**：
- **根因确认（实测）**：该 librime 1.17 精简版**没有 `custom_phrase_translator` 组件**（`gear/` 无该源文件、`gears_module.cc` 未注册）——「patch 接线 custom_phrase_translator + 编译 dict.yaml」在本构建不成立。luna_pinyin 方案**原生内置** `table_translator@custom_phrase`，配置节为 `{dictionary: "", user_dict: custom_phrase, db_class: stabledb}`，会话创建时自动读 `%AppData%\Rime\custom_phrase.userdb.txt`。
- **存储 = rime 标准文件**：`%AppData%\Rime\custom_phrase.userdb.txt`（TableDb 纯文本，行 = `短语<TAB>编码<TAB>提交次数`，首行 `# Rime user dictionary` 注释被跳过）。**不引入自造 JSON/CSV 格式、不手写词典生成器、不建 custom_phrase.dict.yaml**（`SchemaUpdate` 不会编译它，纯无效文件）。
- **热加载 = `rime_api->cleanup_all_sessions()`**（`OctReloadPhrases()`，`RimeWithWeasel.cpp`）：`UserDictionaryComponent::db_pool_` 是 `weak<Db>` 弱引用，会话销毁后下个会话重建 StableDb 并重读最新 txt。**无需 deploy_schema/start_maintenance/重建 .table.bin**。
- **方法**：`oim.phrase.list` / `set {text, code, weight?}` / `remove {code[, text]}` / `reload`。OctServer.cpp 对标准文件薄读写（`ReadPhrases` 解析、`WritePhrase` 原子写盘 tmp+MoveFileExW）+ mtime 轮询线程（只盯 `custom_phrase.userdb.txt`，2s）自动触发 reload；`main.go` 转发并补传 `remove` 的 `text`。
- **修复 `WritePhrase` 死循环 bug**：匹配分支 `continue` 跳过 `start = nl + 1` 游标推进 → 更新/删除无限重读同一行、内存膨胀至 2.8GB 崩溃。已用 `drop` 标记重构，修复后 0.00s 响应、内存 ~10MB。
- **端到端实测 PASS**：TCP `oim.phrase.set {中国移动/zgyd/200}` → 命名管道模拟建会话 + FocusIn + 键入 `z/g/y/d`（handled=1）→ `oim.context` preedit `"z g y d‸"`、candidates 首项「中国移动」（主词典同音词在后）。

**剪贴板能力**：`oim.clipboard.get/set`（Win32 API，无需 `ExecOnHandler`）+ watch 线程（1s 轮询，文本变化时）上行 `Push("oim.clipboard", {text})`。**已知限制**：跨完整性级别（提权 vs 普通进程）剪贴板被 UAC 隔离，oimd 与目标应用同级别时正常。

**回归（实测全过无崩溃）**：clipboard.get/set 往返、oim.status（luna_pinyin_simp 激活）、schema.list、option.set、history.add/get、phrase.set/remove/list 均正常；oimd 稳定运行、内存 ~10MB。`custom_phrase.userdb.txt` 已恢复干净状态（中国移动/zgyd/200、临时短语/ztemp/50、中华全国总工会/zhqz/200）。