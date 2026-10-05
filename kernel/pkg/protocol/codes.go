package protocol

// 诊断码（error.data.diag）——「理想架构.md §19」：与数字码并存。
// 数字码走线上协议供程序分支判断；诊断码供日志检索/UI 文案映射/文档引用。
// pkg/protocol 是唯一真源：SDK 与 UI 应从此派生常量，不得各自硬编码字符串。

const (
	DiagParse            = "E_PARSE"
	DiagMethodNotFound   = "E_METHOD_NOT_FOUND"
	DiagPluginDown       = "E_PLUGIN_DOWN"
	DiagTimeout          = "E_TIMEOUT"
	DiagPluginCrashed    = "E_PLUGIN_CRASHED"
	DiagPluginMissing    = "E_PLUGIN_MISSING"
	DiagPluginState      = "E_PLUGIN_STATE"
	DiagPermissionDenied = "E_PERMISSION_DENIED"
	DiagHotkeyConflict   = "E_HOTKEY_CONFLICT"
	DiagDepsMissing      = "E_DEPS_MISSING"
	DiagDepsInstallFail  = "E_DEPS_INSTALL_FAILED"
	DiagStreamClosed     = "E_STREAM_CLOSED"
	DiagIO               = "E_IO"
	DiagHandshakeTimeout = "E_HANDSHAKE_TIMEOUT"
	DiagHandshakeReject  = "E_HANDSHAKE_REJECTED"
	DiagStdoutContam     = "E_STDOUT_CONTAMINATED"
	DiagDepsHashMismatch = "E_DEPS_HASH_MISMATCH"
	DiagBusyTimeout      = "E_BUSY_TIMEOUT"
	DiagToolUnavailable  = "E_TOOL_UNAVAILABLE"
	DiagModelLoadTimeout = "E_MODEL_LOAD_TIMEOUT"
	DiagVRAMInsufficient = "E_VRAM_INSUFFICIENT"
	// DiagModelNotReady：模型尚未由宿主加载（转化域 §19.6）。
	DiagModelNotReady = "E_MODEL_NOT_READY"
)

// 方法名常量：宿主/插件/内核共同引用的 JSON-RPC method。
const (
	MethodHandshake = "$/handshake"
	MethodPing      = "ping"

	MethodKernelHello = "kernel.hello"
	MethodKernelPing  = "kernel.ping"

	MethodPluginList         = "plugin.list"
	MethodPluginDetails      = "plugin.details"
	MethodPluginStart        = "plugin.start"
	MethodPluginCall         = "plugin.call"
	MethodPluginRestart      = "plugin.restart"
	MethodPluginImport       = "plugin.import"
	MethodPluginRemove       = "plugin.remove"
	MethodPluginGetLifecycle = "plugin.getLifecycle"
	MethodPluginGetSettings  = "plugin.getSettings"
	MethodPluginSetSettings  = "plugin.setSettings"
	MethodPluginUpdateSet    = "plugin.updateSettings"

	// 插件侧请求方法（插件→内核，经 dispatchGate 处理）。
	MethodSDKBusy = "sdk.busy"

	MethodPermsList     = "perms.list"
	MethodPermsAuthor   = "perms.authorize"
	MethodPermsRevoke   = "perms.revoke"
	MethodRegistryList  = "registry.list"
	MethodRegistryCall  = "registry.call"
	MethodCommandList   = "command.list"
	MethodDepsPreview   = "deps.preview"
	MethodDepsInstall   = "deps.install"
	MethodDepsGetConfig = "deps.getConfig"
	MethodDepsSetConfig = "deps.setConfig"
	MethodDepsEnvs      = "deps.envs"

	MethodGateFileRead   = "gate.file_read"
	MethodGateFileExit   = "gate.file_exists"
	MethodGateModelReady = "gate.model_ensure"
	MethodGateSettingsGet = "gate.settings_get" // §17.2 A：插件读「自己的」生效设置（plugins[id].settings）
	MethodGateSettingsSet = "gate.settings_set" // §17.2 A：插件写「自己的」设置（仅 schema 声明字段）
	MethodUISettingsGet  = "ui.getSettings"
	MethodUISettingsSet  = "ui.setSettings"
	MethodEventEmit      = "event.emit"
)