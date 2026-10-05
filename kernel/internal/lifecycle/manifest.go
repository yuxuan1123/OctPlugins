package lifecycle

import (
	"encoding/json"
	"strings"
)

// FR-1 描述文件声明（schema v1）。MVP 先落地 permissions / dependencies / commands。
// 对齐《理想架构.md》§6/§7：profiles / codeDeps / externalDependencies / settingsSchema / defaults。

// DepDecl 声明一项依赖文件及其处理方式（FR-3 自动识别）。
type DepDecl struct {
	Path     string `json:"path"`               // 如 requirements.txt、package.json、go.mod、Cargo.toml
	Manager  string `json:"manager"`            // python/uv、npm、go、cargo、maven、bundler、dotnet
	Explicit bool   `json:"explicit,omitempty"` // true=必须显式确认（FR-3）
}

// CommandDecl 声明一条命令（FR-9 命令面板自动补全）。
type CommandDecl struct {
	Name   string         `json:"name"` // 如 /translate
	Desc   string         `json:"desc"`
	Method string         `json:"method,omitempty"` // 可选：执行目标=本插件内方法；空则仅作展示
	Params []CommandParam `json:"params,omitempty"`
}

type CommandParam struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`            // string/int/bool/string[]
	Needs []string `json:"needs,omitempty"` // 提供给该参数的候选，如语言列表
}

// FnDecl 声明一个可供宿主/其他插件按名调用的共享函数（FR-8，经内核中转）。
type FnDecl struct {
	Name   string `json:"name"`   // 全局唯一，如 np1.version
	Method string `json:"method"` // 插件内响应该方法名
	Desc   string `json:"desc,omitempty"`
}

// HotkeyDecl 声明一个全局热键（对齐宿主 main.ts scanDeclaredHotkeys 读取的字段）。
// 宿主用 globalShortcut 注册并触发 method/action；内核仅承载 schema（§7.5 严格解析需认识该字段）。
type HotkeyDecl struct {
	ID        string            `json:"id"`
	Name      string            `json:"name,omitempty"`
	Combo     string            `json:"combo,omitempty"` // 如 "Alt+X"
	Scope     string            `json:"scope,omitempty"` // global | app
	Method    string            `json:"method,omitempty"`
	Action    string            `json:"action,omitempty"`
	Params    map[string]any    `json:"params,omitempty"` // 触发时传给 method 的参数
	TimeoutMs int               `json:"timeoutMs,omitempty"`
	Desc      string            `json:"desc,omitempty"`
	Window    *HotkeyWindowDecl `json:"window,omitempty"` // 触发时打开/聚焦的呈现窗（通用，宿主零硬编码）
}

// HotkeyWindowDecl 热键触发时呈现给用户的窗口声明（相对插件根的 page + 尺寸；singleton 时
// 已开着则仅聚焦，不再重复开窗/触发）。宿主按此声明通用建窗，不识别任何插件专用字段。
type HotkeyWindowDecl struct {
	Page      string `json:"page"`                // 相对插件根，如 "ui/result_win.html"
	Title     string `json:"title,omitempty"`     // 窗口标题
	Width     int    `json:"width,omitempty"`     // 宽（默认 480）
	Height    int    `json:"height,omitempty"`    // 高（默认 320）
	Frameless bool   `json:"frameless,omitempty"` // 无边框自绘标题栏（注入 editor-preload）
	Singleton *bool  `json:"singleton,omitempty"` // 默认 true：同热键窗已开则聚焦
}

// UIDecl 声明插件的 iframe 网页界面（阶段G：M3 iframe 插件页）。
// Type=web 表示宿主用 iframe 加载该插件自带 HTML UI；空则插件为无界面后端。
type UIDecl struct {
	Type  string `json:"type"`  // "web"
	Entry string `json:"entry"` // 相对插件根目录，如 "ui/index.html"
}

// ProfileDecl 一个具名启动 profile（§7.1）。
// 宿主派生插件前先读 profile 的 requiresResources（静态声明）做资源就绪，再 spawn。
type ProfileDecl struct {
	Entry             string         `json:"entry,omitempty"`
	Args              map[string]any `json:"args,omitempty"`
	RequiresResources []string       `json:"requiresResources,omitempty"`
	StartTimeoutMs    int            `json:"startTimeoutMs,omitempty"`
	Transport         string         `json:"transport,omitempty"`
	// Net 该 profile 是否允许联网（转化域 §23.3：m3u8 远程拉取做独立可选 profile）。
	// nil = 继承 manifest 级 Net；显式 false/true 覆盖之。
	Net *bool `json:"net,omitempty"`
	// Optional 该 profile 为可选能力：宿主不默认启动，仅在用户显式选择时使用。
	Optional bool   `json:"optional,omitempty"`
	Desc     string `json:"desc,omitempty"`
}

// RuntimeDecl 代码依赖的运行时声明（§6.1 codeDeps.runtime）。
type RuntimeDecl struct {
	Language string `json:"language,omitempty"`
	Version  string `json:"version,omitempty"`
}

// CodeDepsDecl 代码依赖声明（§6.1）。lockfile 为提交进版本库的锁定文件（如 start.lock）。
type CodeDepsDecl struct {
	Runtime  RuntimeDecl         `json:"runtime,omitempty"`
	Deps     map[string][]string `json:"deps,omitempty"` // 语言 → 依赖行，如 {"python": ["requests>=2.31"]}
	Lockfile string              `json:"lockfile,omitempty"`
}

// ExternalDep 外部依赖声明（§7 externalDependencies）。kind ∈ tool | model。
type ExternalDep struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Optional     bool   `json:"optional,omitempty"`
	Backend      string `json:"backend,omitempty"`
	Locate       string `json:"locate,omitempty"`       // tool：bundled / explicitPath / env（§14.2）
	DeclaredPath string `json:"declaredPath,omitempty"` // tool：explicitPath/bundled 的声明路径
}

// OptLocate 返回 §14.2 定位方案；未声明回落 env（默认）。
func (d ExternalDep) OptLocate() string {
	if strings.TrimSpace(d.Locate) == "" {
		return "env"
	}
	return d.Locate
}

// StartPolicyDecl 启动时机（§9.1 trigger：onAppStart/onUiReady/onUserClick/onIdleDelay）。
type StartPolicyDecl struct {
	Trigger string `json:"trigger,omitempty"`
}

// StopPolicyDecl 关闭时机（§9.1 mode：resident/idleTimeout/predictive/onDemand）。
type StopPolicyDecl struct {
	Mode        string `json:"mode,omitempty"`
	IdleMinutes int    `json:"idleMinutes,omitempty"`
}

// DefaultsDecl 作者默认策略（§7 defaults），仅可被 user-settings.json 覆盖。
type DefaultsDecl struct {
	Profile     string           `json:"profile,omitempty"`
	StartPolicy *StartPolicyDecl `json:"startPolicy,omitempty"`
	StopPolicy  *StopPolicyDecl  `json:"stopPolicy,omitempty"`
}

// ConversionDecl 一条转换边声明（转化域 §二：边由 tool 声明，outtool 无 manifest 不参与建图）。
// from/to 为格式标识（小写、不带点，如 "md"/"docx"）。conversion 插件汇总全部 tool 的边构建有向图。
// 纯工具边无需模型；能力边（ocr/tts/stt/translate）标 requiresCapability。
type ConversionDecl struct {
	From       []string `json:"from"`
	To         []string `json:"to"`
	Method     string   `json:"method,omitempty"`             // 处理该边的插件方法（缺省 "convert"）
	Capability string   `json:"requiresCapability,omitempty"` // 可选项：ocr/tts/stt/translate
	Note       string   `json:"note,omitempty"`
}

// ProvidesModelDecl 模型提供声明（转化域 §三）：模型由 tool / outtool 声明提供，宿主登记，
// 宿主加载（tool 只声明 + 请求，不自行加载权重）。不做目录扫描发现。
//
// Default / Fallbacks 服务于《理想架构.md》§15.4 的解析优先级链（转化域 §9.2 修订后）：
//
//	用户 pin > 作者默认(default) > 量化变体 > 作者 fallbacks > 宿主能力兜底
//
// 二者均按 capability 维度解释：一个 tool 可以为同一 capability 声明多个模型，
// 其中至多一个标 default=true（作者认定的默认路径），其余按 fallbacks 排序备用。
type ProvidesModelDecl struct {
	ID          string   `json:"id"`
	Capability  string   `json:"capability,omitempty"` // 可选项：ocr/tts/stt/translate
	Backend     string   `json:"backend,omitempty"`
	Path        string   `json:"path,omitempty"` // 相对模型根，如 <modelId>/<quant>/（§20.5）
	Quant       string   `json:"quant,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	SizeBytes   int64    `json:"sizeBytes,omitempty"`
	MinVramGB   float64  `json:"minVramGB,omitempty"`
	License     string   `json:"license,omitempty"`
	QualityTier string   `json:"qualityTier,omitempty"`
	ColdStartMs int      `json:"coldStartMs,omitempty"`
	// Default 标记该 capability 的作者默认模型（§15.4「作者声明的模型（默认路径）」）。
	// 同一 capability 至多一个为 true；多个或全无时宿主按「最快」兜底并记诊断。
	Default bool `json:"default,omitempty"`
	// Fallbacks 作者声明的降级模型 id 序列（§15.4；§12.4 慢模型失败时按此回落）。
	Fallbacks []string `json:"fallbacks,omitempty"`
	// Companion 标记「伴随模型」：与同 capability 的主模型一起构成一个不可分割的模型组
	// （如 RapidOCR 的 det + rec、MOSS-TTS 的 codec 依赖）。
	// 伴随模型不参与 §15.4 默认选择、不计入 §10.1 的能力维度上限——它随主模型一起加载。
	Companion bool `json:"companion,omitempty"`

	// DownloadURL 首次下载源（转化域 §26：首次下载须同意 + 后台拉取 + 进度，**下载由宿主执行**）。
	// 为空表示该模型不提供自动下载（用户需手动放置权重）——UI 如实提示，不伪造可下载。
	DownloadURL string `json:"downloadUrl,omitempty"`
	// DownloadSHA256 下载产物的 sha256（小写 hex）；非空时宿主下载后校验，不匹配则丢弃。
	DownloadSHA256 string `json:"downloadSha256,omitempty"`
}

// NativeLibDecl 原生共享库声明（转化域 §20.4）。
//
// 为什么需要它：`lockHash` 锁的是代码依赖（requirements.txt / package.json 等），
// **锁不住 `.dll` / `.so` / `.dylib`**——而 cgo/purego 类 tool（如 RapidOCR dlopen
// onnxruntime）的行为完全由这些库决定。故作者 MUST 在此显式声明原生库清单。
//
// 校验语义：
//   - SHA256 非空 → 宿主在 spawn 前校验文件哈希，不匹配即拒绝启动（E_DEPS_HASH_MISMATCH）；
//     哈希不匹配**永不**容忍，即使 Optional=true —— 库在但内容不对是最危险的情形；
//   - SHA256 为空 → 仅登记留痕（用于「库由系统/包管理器提供、跨机器哈希不固定」的场景）；
//   - Optional 只容忍「库不存在」：作者随包分发时 MUST 置 false 并填 sha256（严格），
//     库由用户/系统提供时置 true（缺失降级为运行时 E_TOOL_UNAVAILABLE，不阻断插件启动）。
//
// Path 支持 `env:NAME` 形式：取该环境变量的值作为路径，便于用户在设置里指定自备库。
type NativeLibDecl struct {
	Path     string `json:"path"`             // 绝对路径，或 env:VARNAME
	SHA256   string `json:"sha256,omitempty"` // 小写 hex；空 = 只登记不校验
	Optional bool   `json:"optional,omitempty"`
	Note     string `json:"note,omitempty"`
}

// ManifestFields 嵌入 Manifest 的声明式字段。
type ManifestFields struct {
	Permissions  []string      `json:"permissions"`
	Dependencies []DepDecl     `json:"dependencies"`
	Commands     []CommandDecl `json:"commands"`
	Functions    []FnDecl      `json:"functions"`
	UI           UIDecl        `json:"ui,omitempty"`
	Hotkeys      []HotkeyDecl  `json:"hotkeys,omitempty"` // §10.3：宿主 globalShortcut 注册（内核承载 schema）

	// 对齐《理想架构.md》§6/§7 的声明字段（MVP 宿主先承载声明，策略落地见二期）。
	Profiles       map[string]ProfileDecl `json:"profiles,omitempty"`
	CodeDeps       CodeDepsDecl           `json:"codeDeps,omitempty"`
	ExternalDeps   []ExternalDep          `json:"externalDependencies,omitempty"`
	SettingsSchema json.RawMessage        `json:"settingsSchema,omitempty"` // 透传给宿主渲染表单（§7.4）
	Defaults       DefaultsDecl           `json:"defaults,omitempty"`

	// 转化域声明（conversion.md §二/§三）：转换边与模型提供。二者均为可选，
	// 仅对声明了它们的 tool 生效；宿主严格解析（§7.5）故 MUST 与内核结构体同步。
	Conversions    []ConversionDecl    `json:"conversions,omitempty"`
	ProvidesModels []ProvidesModelDecl `json:"providesModels,omitempty"`

	// NativeLibs 原生共享库清单（转化域 §20.4）：纳入 §20 校验，补 lockHash 锁不住 .so/.dll 的缺口。
	NativeLibs []NativeLibDecl `json:"nativeLibs,omitempty"`

	// 转化域 §23：conversion 插件的 manifest 契约。
	//
	// RequiresCapabilities §23.1：声明「本插件需要哪些能力维度」，**不写具体 model id**
	// （model id 是宿主按 §15.4 解析的结果，作者不该也不需要在 manifest 里固化）。
	// 内核据此约束 gate.model_ensure：只能请求已声明的能力，防止插件越权索取资源。
	RequiresCapabilities []string `json:"requiresCapabilities,omitempty"`
	// Spawn §23.2：可派生的 outtool id 白名单（pandoc / soffice / ffmpeg / resvg）。
	// 非空时，白名单中的 id MUST 已在 externalDependencies 里声明（登记期一致性校验）。
	Spawn []string `json:"spawn,omitempty"`
	// Net §23.2：是否允许联网，默认 false。声明为 true 时 MUST 同时声明 network 权限。
	Net bool `json:"net,omitempty"`
}
