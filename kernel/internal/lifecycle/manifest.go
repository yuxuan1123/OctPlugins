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

// ManifestFields 嵌入 Manifest 的声明式字段。
type ManifestFields struct {
	Permissions  []string      `json:"permissions"`
	Dependencies []DepDecl     `json:"dependencies"`
	Commands     []CommandDecl `json:"commands"`
	Functions    []FnDecl      `json:"functions"`
	UI           UIDecl        `json:"ui,omitempty"`

	// 对齐《理想架构.md》§6/§7 的声明字段（MVP 宿主先承载声明，策略落地见二期）。
	Profiles       map[string]ProfileDecl `json:"profiles,omitempty"`
	CodeDeps       CodeDepsDecl           `json:"codeDeps,omitempty"`
	ExternalDeps   []ExternalDep          `json:"externalDependencies,omitempty"`
	SettingsSchema json.RawMessage        `json:"settingsSchema,omitempty"` // 透传给宿主渲染表单（§7.4）
	Defaults       DefaultsDecl           `json:"defaults,omitempty"`
}
