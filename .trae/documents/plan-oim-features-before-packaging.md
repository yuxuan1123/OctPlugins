# OIM 打包前补功能：自定义快捷输入 + 剪贴板能力 + 端口/部署收尾

## Context（背景）

用户决定**先补功能、后做 Phase 4 打包**（避免二次打包）。Phase 1-3 已验收，本次在打包前完成三项 oim 侧功能：

1. **自定义快捷输入**（扩展点 #5，纯 oim 本地）：**复刻 rime 原生 `custom_phrase` 机制**——用户/OctPlugins 直接编辑标准词典文件 `custom_phrase.dict.yaml`，热加载走 rime 官方部署 API。**不引入自造 JSON 格式、不自造词典生成器**（响应"不要重复造轮子"）。
2. **剪贴板能力**（扩展点 #2）：oimd 本地读写系统剪贴板 + 监听变更 + 上行推送，供 OctPlugins 消费。
3. **端口/部署收尾**：统一端口常量、启动脚本整理、配置规范化。

已核实的约束（**实测修订版**）：
- **该 librime 1.17 精简版没有 `custom_phrase_translator` 组件**（`gear/` 目录无 custom_phrase_translator.cc，`gears_module.cc` 注册的翻译器列表里也没有它）——所以「patch 接线 custom_phrase_translator + 编译 dict.yaml」这条路在本构建根本不成立。
- **luna_pinyin 方案原生已内置 `table_translator@custom_phrase`**，其 `custom_phrase` 配置节是 `{dictionary: ""(不编译词典), user_dict: custom_phrase, db_class: stabledb}`。短语来自 `%AppData%\Rime\custom_phrase.userdb.txt`（stabledb 纯文本，运行时读全文）。
- `SchemaUpdate::Run`（deployment_tasks.cc）只编译主 `translator/dictionary`，**不会编译 `custom_phrase/dictionary`** → 无需任何 schema patch、无需 deploy_schema/start_maintenance。
- **热加载只需 `rime_api->cleanup_all_sessions()`**：`UserDictionaryComponent::db_pool_` 是 `weak<Db>` 弱引用，会话销毁后下个会话重建 StableDb 并重读最新 txt。毫秒级。
- `ExecOnHandler` 回调在调用线程同步执行（OctServer.cpp 已在用）：reload 放独立工作线程，避免阻塞 TCP 响应。
- 剪贴板能力不涉及 librime，纯放 `OctServer.cpp`（Win32 API），无需 `ExecOnHandler`；上行通知复用现成的 `Push(method, paramsJson)`。
- 用户数据目录 = `%AppData%\Rime`（[WeaselUtility.cpp](file:///d:/Project/ElectronProject/OctPlugins/mvp_test/oim/weasel/RimeWithWeasel/WeaselUtility.cpp#L6-L25)）。共享数据目录 = oimd.exe 同目录 `\data`（只读）。

## 实施方案

### 1. 自定义快捷输入（oimd 侧）—— 复刻 rime 原生 `custom_phrase`，不重复造轮子

> **原则**：快捷输入是 rime 自带能力（`table_translator@custom_phrase` + stabledb 用户词典）。我们不建第二套存储格式、不手写词典生成器、不碰 librime 内部——只做两件事：**对标准 stabledb 文件 `custom_phrase.userdb.txt` 的薄读写**（供程序化增删）与 **`cleanup_all_sessions()` 热加载**。

**短语存储（= rime 标准文件，零自造）**：`%AppData%\Rime\custom_phrase.userdb.txt`。luna_pinyin 原生 `custom_phrase` 配置 `{dictionary: "", user_dict: custom_phrase, db_class: stabledb}` 在会话创建时自动读它。文件为 TableDb 纯文本格式（行 = `短语<TAB>编码<TAB>提交次数`）：

```
# Rime user dictionary
中国移动	zgyd	100
中华全国总工会	zhqz	200
```

**无需任何接线**：luna_pinyin 方案原生已带该翻译器与配置，**不要**在 `luna_pinyin.custom.yaml` 里 patch `custom_phrase_translator`（该组件本构建不存在，会污染 schema），**不要**建 `custom_phrase.dict.yaml`（SchemaUpdate 不会编译它，纯属无效文件）。首启 `EnsurePhraseConfig()` 幂等清理以上两类残留 + 确保 userdb.txt 存在。

**热加载 = `rime_api->cleanup_all_sessions()`**（`OctReloadPhrases()`，在 `RimeWithWeasel.cpp`）：会话销毁后 stabledb 弱引用失效，下个会话重建并重读 txt。**不需要** `deploy_schema`/`start_maintenance`/重建 .table.bin。毫秒级，仍放工作线程（`RequestPhraseReload` 排重调度 + `ExecOnHandler`）避免阻塞 TCP 响应。

**对标准文件的薄读写封装**（存储仍是 rime 标准文件，不做第二套格式）：
- `oim.phrase.list` → 解析 `custom_phrase.userdb.txt` 返回短语数组 `[{code,text,weight}]`
- `oim.phrase.set {text, code, weight?}` → 按 `编码+短语` 幂等替换/追加 `text\tcode\tweight` 行 + 原子写盘 + reload
- `oim.phrase.remove {code[, text]}` → 带 text 删单条、仅 code 删整组 + 写盘 + reload
- `oim.phrase.reload` → 仅触发 cleanup_all_sessions（用户手动编辑文件后调用）

**自动热加载（保留）**：fork 的 librime **无原生文件监视器**。保留轻量 mtime 轮询线程，**只监视 rime 标准文件 `custom_phrase.userdb.txt`**（不引入 oct_phrases.json），变化时触发与 `oim.phrase.reload` 相同的流程。

**Go 插件**：`main.go` 转发 `phrase.list/set/remove/reload`（`remove` 带 `text`）；manifest 已加对应 function 声明。

### 2. 剪贴板能力（oimd 侧，纯 OctServer）

`OctServer.cpp` 新增（Win32，无需 ExecOnHandler）：
- `oim.clipboard.get` → `{"text":...}`（当前剪贴板文本）
- `oim.clipboard.set {text}` → 写入剪贴板
- 后台 watch 线程（每 1s 读一次，文本变化时）→ `Push("oim.clipboard", {"text":...})` 上行通知

Go 插件：`main.go` 转发 `clipboard.get/set`，`readOimd` 已自动把上行通知转 `event.emit`（`oim.clipboard` 类型）。manifest 新增 `oim.clipboard.get/set`。

**已知限制**（文档注明）：提权进程与普通进程间剪贴板被 UAC 隔离（OpenClipboard/GetClipboardData 跨完整性级别返回空）；oimd 与目标应用同完整性级别时功能正常。这是 Phase 4 打包时"oimd 以普通权限自启"的一个理由。

### 3. 端口/部署收尾

- 端口统一确认：OctServer 默认 56619 + Go `oimdAddr=127.0.0.1:56619` 一致；无改动。
- `start_service.bat`/`stop_service.bat` 内容已正确（拉起/退出 oimd）。
- 文档规范化：把 `oim.clipboard.*`/`oim.phrase.*` 方法、短语文件格式、剪贴板 UAC 限制、热重载说明写入 oim.md 与通道文档。

## 涉及文件

- 修改 `mvp_test\oim\weasel\WeaselServer\OctServer.cpp` / `OctServer.h`：phrase 薄读写（`PhraseDictPath` → `%AppData%\Rime\custom_phrase.userdb.txt`；`EnsurePhraseConfig` 清理 legacy 接线残留；`ReadPhrases`/`WritePhrase` 操作标准 stabledb 文件）+ mtime 监视（只盯 `custom_phrase.userdb.txt`）+ 剪贴板方法/线程
- 修改 `mvp_test\oim\weasel\RimeWithWeasel\RimeWithWeasel.cpp` / `include\RimeWithWeasel.h`：`OctReloadPhrases()` 仅 `rime_api->cleanup_all_sessions()`（stabledb 弱引用失效后重建并重读 txt，无需 deploy）
- 修改 `plugins\oim\main.go`（转发 phrase/clipboard 方法，`remove` 补传 `text`）
- 修改 `plugins\oim\manifest.json`（新增 function 声明）
- 文档 `mvp_test\oim\oim.md`、`.trae\documents\oim-weasel-ui-horizontal-and-octchannel.md`

## 验证

1. 重编 oimd.exe（`WeaselServer.vcxproj`，传 `/p:SolutionDir`）+ 重编 oim.exe。
2. **短语（端到端实测 PASS）**：TCP `oim.phrase.set {text:"中国移动",code:"zgyd",weight:200}` → 命名管道模拟建会话 + FocusIn + 键入 `z/g/y/d`（handled=1）→ TCP `oim.context` 候选区 preedit `"z g y d‸"`、candidates 首项「中国移动」（权重 200 排第一），其后为主词典同音词（忠肝义胆/主观臆断/中国药典/中共一大）。证明 userdb.txt → 引擎 → 候选全链路打通。
3. **短语热加载（实测）**：直接编辑标准 `%AppData%\Rime\custom_phrase.userdb.txt` → mtime 监视线程自动触发 `cleanup_all_sessions()` 生效，无需 deploy/deploy_schema/重建 .table.bin。
4. **剪贴板**：`oim.clipboard.set {text:"x"}` 后系统粘贴出 `x`；复制文本后 1s 内收到 `oim.clipboard` 上行通知。
5. **回归（实测全过无崩溃）**：clipboard.get/set 往返、oim.status（luna_pinyin_simp 激活）、schema.list、option.set、history.add/get、phrase.set/remove/list 均正常；oimd 稳定运行，内存 ~10MB。
6. 回写文档。

## 明确不做（本次范围外）

- 不做 Phase 4 打包/静默注册（用户明确先功能后打包）。
- 不做云端同步、文件互传、连接服务器抽象（扩展点 #1/#3/#4 归 OctPlugins 主体）。
