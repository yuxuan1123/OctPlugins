package lifecycle

import (
	"errors"
	"fmt"
	"strings"
)

// 本文件：插件进程生命周期策略（第一阶段：数据结构 + 默认值 + 校验）。
// 对应《首要任务.md》第一阶段 1~3 项。字段只在 manifest 层定义并被加载/校验；
// 真正按 load_mode 分级启动 / 懒启动 / 空闲回收 / 退避重启在第二阶段 supervisor 落地。

// ---- load_mode 枚举 ----
const (
	// LoadModeAlways 常驻：内核启动即拉起，永不回收。
	LoadModeAlways = "always"
	// LoadModePrewarm 预热（默认）：可预创建/预拉，不作为常驻硬保证。
	LoadModePrewarm = "prewarm"
	// LoadModeLazy 按需：首次被调用/触发时才拉起，空闲后可能回收。
	LoadModeLazy = "lazy"
	// LoadModeDisabled 禁用：内核不启动、不响应调用。
	LoadModeDisabled = "disabled"
)

// 生命周期默认值（毫秒级；快乐与现有心跳常量一致）。
const (
	DefaultLoadMode            = LoadModePrewarm
	DefaultHeartbeatIntervalMs = 5000  // 5s
	DefaultHeartbeatTimeoutMs  = 15000 // 15s
	DefaultRequestTimeoutMs    = 30000 // 30s
	DefaultStdoutLineBytes     = 8 * 1024 * 1024
	DefaultGracefulShutdownMs  = 2000 // 2s
	// Resilience 退避：base=0.5s，上限=30s；窗口 60s 内最多 3 次。
	DefaultAutoRestart         = true
	DefaultMaxRestarts         = 3
	DefaultMaxRestartsWindowMs = 60000
	DefaultBackoffBaseMs       = 500
	DefaultBackoffMaxMs        = 30000
)

// LifecyclePolicy 插件的进程生命周期策略。
// 以匿名方式内嵌进 supervisor.Manifest，其字段在 manifest.json 顶层平铺。
type LifecyclePolicy struct {
	// LoadMode 加载模式（枚举 LoadMode*）。
	LoadMode string `json:"load_mode,omitempty"`
	// Start 启动阶段配置。
	Start StartupCfg `json:"startup,omitempty"`
	// Heartbeat 心跳保活配置。
	Heartbeat HeartbeatCfg `json:"heartbeat,omitempty"`
	// Recycle 空闲回收配置。
	Recycle RecycleCfg `json:"recycle,omitempty"`
	// Resilience 崩溃自愈与退避配置。
	Resilience ResilienceCfg `json:"resilience,omitempty"`
	// Limits 进程资源限制。
	Limits LimitsCfg `json:"limits,omitempty"`
	// Triggers 懒启动触发条件（on_command / on_ui_open / on_file_ext / on_event）。
	Triggers Triggers `json:"triggers,omitempty"`
}

// StartupCfg 启动阶段配置。
type StartupCfg struct {
	// PrewarmPool 预热进程池大小（prewarm 模式预留；当前阶段不落地）。
	PrewarmPool int `json:"prewarm_pool,omitempty"`
	// KeepAlive 常驻锁：true 表示即便 load_mode 允许回收也常驻（临时保活用）。
	KeepAlive bool `json:"keep_alive,omitempty"`
}

// HeartbeatCfg 心跳配置。
type HeartbeatCfg struct {
	// IntervalMs 发送 ping 的间隔（毫秒）。
	IntervalMs int `json:"interval_ms,omitempty"`
	// TimeoutMs 超过该时长未收到 pong 判定 unhealthy（毫秒）。
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// RecycleCfg 空闲回收配置。
type RecycleCfg struct {
	// IdleRecycle 是否允许空闲回收。
	IdleRecycle bool `json:"idle_recycle,omitempty"`
	// MaxIdleMs 空闲多久后回收（毫秒；IdleRecycle=true 时必须 > 0）。
	MaxIdleMs int `json:"max_idle_ms,omitempty"`
	// GracefulShutdownMs 回收时给插件的优雅关闭时限（毫秒）。
	GracefulShutdownMs int `json:"graceful_shutdown_ms,omitempty"`
	// BackgroundTasks 插件是否有后台轮询/定时任务；true 时跳过空闲回收。
	BackgroundTasks bool `json:"background_tasks,omitempty"`
	// StopMode 关闭时机（§9.1 mode）："onDemand"=用完即关（pending 归零立即回收）；
	// "idleTimeout"=空闲到点回收（等价旧 IdleRecycle 语义）；空=沿用 IdleRecycle。
	StopMode string `json:"stop_mode,omitempty"`
}

// ResilienceCfg 崩溃自愈与退避配置。
type ResilienceCfg struct {
	// AutoRestart 崩溃/心跳超时后是否自动重启（默认 true）。
	// 用 *bool 以区分「未声明=默认 true」与「显式改 false」。
	AutoRestart *bool `json:"auto_restart,omitempty"`
	// MaxRestarts 滑动窗口 max_restarts_window_ms 内允许的最大重启次数。
	MaxRestarts int `json:"max_restarts,omitempty"`
	// MaxRestartsWindowMs 重启计数滑动窗口长度（毫秒）。
	MaxRestartsWindowMs int `json:"max_restarts_window_ms,omitempty"`
	// BackoffBaseMs 指数退避基数（毫秒）。
	BackoffBaseMs int `json:"backoff_base_ms,omitempty"`
	// BackoffMaxMs 退避上限（毫秒）。
	BackoffMaxMs int `json:"backoff_max_ms,omitempty"`
	// CrashExitCodes 视为正常退出的退出码列表（不触发自动重启）。
	CrashExitCodes []int `json:"crash_exit_codes,omitempty"`
}

// LimitsCfg 进程资源限制。
type LimitsCfg struct {
	// MemBytes 内存上限（字节）。
	MemBytes int `json:"mem_bytes,omitempty"`
	// CPUPercent CPU 上限（百分比；0=不限制）。
	CPUPercent int `json:"cpu_percent,omitempty"`
	// RequestTimeoutMs 单请求超时（毫秒；跨过则取消该次请求，不杀进程）。
	RequestTimeoutMs int `json:"request_timeout_ms,omitempty"`
	// StdoutLineBytes stdout 单行上限（字节；≤ 8MB 硬限制）。
	StdoutLineBytes int `json:"stdout_line_bytes,omitempty"`
}

// Triggers 懒启动触发条件集合。
type Triggers struct {
	// OnCommand 命令名（如 "/md"），命中命令面板执行时触发拉起。
	OnCommand []string `json:"on_command,omitempty"`
	// OnUIOpen 打开的插件 UI 页面 ID（如 "md-index"、"md-editor"），命中时触发拉起。
	OnUIOpen []string `json:"on_ui_open,omitempty"`
	// OnFileExt 文件后缀（如 ".md"），宿主处理该类型文件时触发拉起。
	OnFileExt []string `json:"on_file_ext,omitempty"`
	// OnEvent 事件类型关键字，命中内核事件广播时触发拉起。
	OnEvent []string `json:"on_event,omitempty"`
}

// HasTriggers 是否声明了任何懒启动触发条件。
func (p *LifecyclePolicy) HasTriggers() bool {
	if p == nil {
		return false
	}
	return len(p.Triggers.OnCommand)+len(p.Triggers.OnUIOpen)+
		len(p.Triggers.OnFileExt)+len(p.Triggers.OnEvent) > 0
}

// boolPtr 返回指向 v 的指针。
func boolPtr(v bool) *bool { return &v }

// ApplyDefaults 为未声明字段填充默认值（幂等，可安全重复调用）。
// 用于旧 manifest 向后兼容：缺失新字段时自动得到合理配置。
func (p *LifecyclePolicy) ApplyDefaults() {
	if p == nil {
		return
	}
	if strings.TrimSpace(p.LoadMode) == "" {
		p.LoadMode = DefaultLoadMode
	}
	if p.Heartbeat.IntervalMs <= 0 {
		p.Heartbeat.IntervalMs = DefaultHeartbeatIntervalMs
	}
	if p.Heartbeat.TimeoutMs <= 0 {
		p.Heartbeat.TimeoutMs = DefaultHeartbeatTimeoutMs
	}
	if p.Limits.RequestTimeoutMs <= 0 {
		p.Limits.RequestTimeoutMs = DefaultRequestTimeoutMs
	}
	if p.Limits.StdoutLineBytes <= 0 {
		p.Limits.StdoutLineBytes = DefaultStdoutLineBytes
	}
	if p.Recycle.GracefulShutdownMs <= 0 {
		p.Recycle.GracefulShutdownMs = DefaultGracefulShutdownMs
	}
	if p.Resilience.AutoRestart == nil {
		p.Resilience.AutoRestart = boolPtr(DefaultAutoRestart)
	}
	if p.Resilience.MaxRestarts <= 0 {
		p.Resilience.MaxRestarts = DefaultMaxRestarts
	}
	if p.Resilience.MaxRestartsWindowMs <= 0 {
		p.Resilience.MaxRestartsWindowMs = DefaultMaxRestartsWindowMs
	}
	if p.Resilience.BackoffBaseMs <= 0 {
		p.Resilience.BackoffBaseMs = DefaultBackoffBaseMs
	}
	if p.Resilience.BackoffMaxMs <= 0 {
		p.Resilience.BackoffMaxMs = DefaultBackoffMaxMs
	}
}

// Validate 校验生命周期策略的合法性。
func (p *LifecyclePolicy) Validate() error {
	if p == nil {
		return nil
	}
	switch p.LoadMode {
	case LoadModeAlways, LoadModePrewarm, LoadModeLazy, LoadModeDisabled:
	default:
		return fmt.Errorf("invalid load_mode %q (want always/prewarm/lazy/disabled)", p.LoadMode)
	}
	if p.Heartbeat.IntervalMs <= 0 {
		return errors.New("heartbeat.interval_ms must be > 0")
	}
	if p.Heartbeat.TimeoutMs < p.Heartbeat.IntervalMs {
		return fmt.Errorf("heartbeat.timeout_ms(%d) must be >= interval_ms(%d)", p.Heartbeat.TimeoutMs, p.Heartbeat.IntervalMs)
	}
	if p.Limits.RequestTimeoutMs <= 0 {
		return errors.New("limits.request_timeout_ms must be > 0")
	}
	if p.Recycle.IdleRecycle && p.Recycle.MaxIdleMs <= 0 {
		return errors.New("recycle.max_idle_ms must be > 0 when recycler.idle_recycle=true")
	}
	if p.Resilience.AutoRestart == nil {
		return errors.New("resilience.auto_restart must be resolved (apply defaults first)")
	}
	if p.Resilience.MaxRestarts < 0 {
		return errors.New("resilience.max_restarts must be >= 0")
	}
	if p.Resilience.BackoffBaseMs <= 0 || p.Resilience.BackoffMaxMs < p.Resilience.BackoffBaseMs {
		return fmt.Errorf("invalid resilience backoff: base=%d max=%d (0 < base <= max)", p.Resilience.BackoffBaseMs, p.Resilience.BackoffMaxMs)
	}
	const hardLimit = 8 * 1024 * 1024
	if p.Limits.StdoutLineBytes > hardLimit {
		return fmt.Errorf("limits.stdout_line_bytes(%d) exceeds hard limit %d", p.Limits.StdoutLineBytes, hardLimit)
	}
	return nil
}
