# Phase 4 · OIM 独立运行 + 随包打包分发

## Context（背景）

Phase 3 双向通道已验收（oim.status/context 临时会话兜底、oim.schema.list、oim.toggle 真实生效、oim.option.set 持久化）。当前 oimd.exe / oimime.dll 仅存在于 `mvp_test\oim\weasel\output\`（开发态），未随 OctPlugins 分发，注册也停留在开发机手动的 system32 注册。

Phase 4 目标：**oimd 独立可打字（优雅降级）+ 随包分发 + 安装/首启静默注册**。

已核实的关键事实：
- 共享数据：`WeaselSharedDataPath()` = 可执行文件目录 + `\data`（[WeaselUtility.cpp](file:///d:/Project/ElectronProject/OctPlugins/mvp_test/oim/weasel/RimeWithWeasel/WeaselUtility.cpp#L27-L31)），即 `oimd.exe` 必须与 `data\` 同目录。
- TSF 自动拉起：oimime.dll 的 `_EnsureServerConnected` 在无 oimd 进程时 ShellExecute **同目录**的 `start_service.bat`（[WeaselTSF.cpp](file:///d:/Project/ElectronProject/OctPlugins/mvp_test/oim/weasel/WeaselTSF/WeaselTSF.cpp#L263-L272)）。
- 静默注册：`regsvr32 /s oimime.dll` 触发 `DllRegisterServer` = RegisterServer + RegisterProfiles + RegisterCategories（[Server.cpp](file:///d:/Project/ElectronProject/OctPlugins/mvp_test/oim/weasel/WeaselTSF/Server.cpp#L103-L109)），CLSID 为 `{A3F4CDED-B1E9-41EE-9CA6-7B4D0DE6CB0A}`。
- 打包：`package.ps1` 整体拷贝 `plugins\` 到 `resources\plugins\`，**无需改动**即可随包分发。
- 开发机现状：TSF CLSID 的 InprocServer32 当前指向 `C:\Windows\System32\oimime.dll`。
- oim Go 插件纯 stdlib（[go.mod](file:///d:/Project/ElectronProject/OctPlugins/plugins/oim/go.mod)），注册逻辑用 `reg.exe`/`regsvr32.exe` 子进程即可，无需引入依赖。

## 实施方案

### 1. 部署包 `plugins\oim\ime\`（随包分发）

新增目录，内容从 `mvp_test\oim\weasel\output\` 同步：

| 拷入 | 说明 |
|---|---|
| `oimd.exe` / `oimime.dll` / `rime.dll` | 服务器 / TSF 客户端 / 引擎 |
| `start_service.bat` / `stop_service.bat` | TSF 自动拉起 oimd 用 |
| `WeaselDeployer.exe` | 首启 best-effort `/install`（可选，失败不阻塞） |
| `data\`（整体） | 共享数据：yaml + `build\` 预编译 + `opencc\` + essay.txt 等 |

**排除**：`*.pdb` `*.lib` `*.exp`、`install.nsi` `install.bat` `uninstall.bat`、`WeaselSetup*.exe`、`7z*` `curl*`、`WinSparkle.dll`、`sudo.js` `check_windows_version.js` `rime-install-config.bat`、`archives\` `Win32\` 目录。

新增 `plugins\oim\prepare_ime.ps1`：robocopy 幂等同步 + 清单校验（oimd.exe/oimime.dll/rime.dll/data 必须存在），可重复执行。

### 2. 静默注册 + 自启（Go 插件首启自动，幂等）

新增 `plugins\oim\deploy.go`，`main()` 启动时调用 `ensureDeployed()`：

- `imeDir` = `os.Executable()` 目录 + `\ime`。
- 幂等标记：`reg query HKCU\Software\Rime\Weasel /v OctDeployPath`，若等于 `imeDir` → 跳过。
- 否则执行部署：
  1. `regsvr32 /s "<ime>\oimime.dll"`（TSF 注册；宿主以管理员运行，HKLM 可写）。
  2. 校验 `HKCR\CLSID\{A3F4CDED-...}\InprocServer32` 指向 `ime\oimime.dll`。
  3. 写 Run 键 `HKCU\...\CurrentVersion\Run\OctOimServer` = `cmd /c start "" "<ime>\oimd.exe"`（登录自启 → 优雅降级）。
  4. best-effort `WeaselDeployer.exe /install`（失败不阻塞）。
  5. 仅当 TCP 连 56619 失败时启动 `ime\oimd.exe`（避免与既有 oimd 端口冲突）。
  6. 写标记 `OctDeployPath = imeDir`。
- 任一步失败仅记 stderr 日志，**不阻塞插件**；重编 `oim.exe`。

### 3. 优雅降级

架构已满足，无需改代码：TSF 客户端自动拉起同目录 `start_service.bat` 中的 oimd.exe；Run 键登录自启。OctPlugins 未运行时 oimd 独立可打字，运行时经 TCP 56619 双向通信。

### 4. 验证

1. `prepare_ime.ps1` 生成 ime\，确认清单存在。
2. Go 重编 `oim.exe`；触发一次部署，确认：
   - InprocServer32 改写为 `plugins\oim\ime\oimime.dll`
   - Run 键出现 `OctOimServer`
   - 标记写入，二次启动跳过
3. TCP 冒烟：oim.status / oim.schema.list / oim.toggle 正常（连接到的 oimd 为 ime\ 下新进程）。
4. 优雅降级：kill oimd → 在记事本切 OCT 输入法，确认 oimd 被自动拉起且能上屏（需用户配合操作系统语言栏）。
5. 打包冒烟：局部拷贝验证 `ime\` 落入 `resources\plugins\oim\ime\`（完整跑 `package.ps1` 可选，较重）。

### 副作用提示（需确认）

开发机 TSF CLSID 现指向 `System32\oimime.dll`；Phase 4 首次部署会把注册改写为 `plugins\oim\ime\oimime.dll`（新唯一来源）。system32 旧 DLL 文件残留但不再被引用，无害。同时当前正在运行的 `mvp_test` 版 oimd 会与 ime\ 版并存至下次重启（端口已被占用时插件不会重复拉起）。

## 涉及文件

- 新增 `plugins\oim\ime\`（部署包，脚本生成）
- 新增 `plugins\oim\prepare_ime.ps1`
- 新增 `plugins\oim\deploy.go`
- 修改 `plugins\oim\main.go`（main() 启动调用 ensureDeployed）
- 文档 `mvp_test\oim\oim.md`、`.trae\documents\oim-weasel-ui-horizontal-and-octchannel.md` 回写 Phase 4
