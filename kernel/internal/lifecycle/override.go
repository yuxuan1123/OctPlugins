package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/octplugin/kernel/internal/registry"
)

// 本文件：第三方（宿主设置 UI）的用户覆盖持久化。
// 用户对插件生命周期策略的修改存到 store/overrides.json（map[pluginID]Override），
// 启动时在 manifest 默认值之上合并覆盖（override 优先）；未覆盖的字段沿用 manifest。

// Override 用户手动覆盖的生命周期字段。指针字段以区分「未设置(nil)」与「显式设置」。
type Override struct {
	LoadMode            *string `json:"load_mode,omitempty"`
	KeepAlive           *bool   `json:"keep_alive,omitempty"`
	PrewarmPool         *int    `json:"prewarm_pool,omitempty"`
	HeartbeatIntervalMs *int    `json:"heartbeat_interval_ms,omitempty"`
	HeartbeatTimeoutMs  *int    `json:"heartbeat_timeout_ms,omitempty"`
	IdleRecycle         *bool   `json:"idle_recycle,omitempty"`
	MaxIdleMs           *int    `json:"max_idle_ms,omitempty"`
	GracefulShutdownMs  *int    `json:"graceful_shutdown_ms,omitempty"`
	BackgroundTasks     *bool   `json:"background_tasks,omitempty"`
	MemBytes            *int    `json:"mem_bytes,omitempty"`
	CPUPercent          *int    `json:"cpu_percent,omitempty"`
	RequestTimeoutMs    *int    `json:"request_timeout_ms,omitempty"`
	StdoutLineBytes     *int    `json:"stdout_line_bytes,omitempty"`
	AutoRestart         *bool   `json:"auto_restart,omitempty"`
	MaxRestarts         *int    `json:"max_restarts,omitempty"`
	MaxRestartsWindowMs *int    `json:"max_restarts_window_ms,omitempty"`
	BackoffBaseMs       *int    `json:"backoff_base_ms,omitempty"`
	BackoffMaxMs        *int    `json:"backoff_max_ms,omitempty"`
	CrashExitCodes      []int   `json:"crash_exit_codes,omitempty"`
}

// overridesFile 持久化文件结构：map[pluginID]Override。
type overridesFile map[string]Override

// manager 需要的额外状态（在 manager.go 的 Manager 上扩展字段，见下面的方法）。

// SetStoreDir 绑定持久化目录（store/）。启动时扫描并加载用户覆盖。
func (m *Manager) SetStoreDir(dir string) {
	m.mu.Lock()
	m.storeDir = dir
	m.mu.Unlock()
	m.loadOverrides()
}

// SetRegistryStore 注入 §8 registry.json 唯一写入方（内核装配时调用）。
func (m *Manager) SetRegistryStore(s *registry.Store) {
	m.mu.Lock()
	m.regStore = s
	m.mu.Unlock()
}

// loadOverrides 从 state/overrides.json 读取所有插件的用户覆盖。文件不存在视为空。
func (m *Manager) loadOverrides() {
	m.mu.Lock()
	dir := m.storeDir
	m.mu.Unlock()
	if dir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(dir, "overrides.json"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[kernel] read overrides.json: %v", err)
		}
		return
	}
	var f overridesFile
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("[kernel] parse overrides.json: %v", err)
		return
	}
	m.mu.Lock()
	m.overrides = f
	m.mu.Unlock()
}

// OverrideFor 返回某插件的用户覆盖（无则空 Override）。
func (m *Manager) OverrideFor(id string) Override {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overrides == nil {
		return Override{}
	}
	return m.overrides[id]
}

// SaveOverride 更新某插件的用户覆盖并立即持久化。
// 只保留非 nil 字段；传 nil 表示清空该插件所有覆盖。
func (m *Manager) SaveOverride(id string, ov Override) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overrides == nil {
		m.overrides = overridesFile{}
	}
	dirty := ov.hasAny()
	if dirty {
		m.overrides[id] = ov
	} else {
		delete(m.overrides, id)
	}
	return m.persistOverridesLocked()
}

func (m *Manager) persistOverridesLocked() error {
	if m.storeDir == "" {
		return errors.New("store dir not configured")
	}
	b, err := json.MarshalIndent(m.overrides, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.storeDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.storeDir, "overrides.json"), b, 0o644)
}

// hasAny 是否有任一字段被设置。
func (o Override) hasAny() bool {
	return o.LoadMode != nil || o.KeepAlive != nil || o.PrewarmPool != nil ||
		o.HeartbeatIntervalMs != nil || o.HeartbeatTimeoutMs != nil ||
		o.IdleRecycle != nil || o.MaxIdleMs != nil || o.GracefulShutdownMs != nil ||
		o.BackgroundTasks != nil || o.MemBytes != nil || o.CPUPercent != nil ||
		o.RequestTimeoutMs != nil || o.StdoutLineBytes != nil || o.AutoRestart != nil ||
		o.MaxRestarts != nil || o.MaxRestartsWindowMs != nil || o.BackoffBaseMs != nil ||
		o.BackoffMaxMs != nil || o.CrashExitCodes != nil
}

// apply 把覆盖的字段写进一个 LifecyclePolicy 副本（未覆盖字段保持原有值）。
// 返回覆盖后的副本；原传入策略不被修改。
func (o Override) apply(p LifecyclePolicy) LifecyclePolicy {
	if o.LoadMode != nil {
		p.LoadMode = *o.LoadMode
	}
	if o.KeepAlive != nil {
		p.Start.KeepAlive = *o.KeepAlive
	}
	if o.PrewarmPool != nil {
		p.Start.PrewarmPool = *o.PrewarmPool
	}
	if o.HeartbeatIntervalMs != nil {
		p.Heartbeat.IntervalMs = *o.HeartbeatIntervalMs
	}
	if o.HeartbeatTimeoutMs != nil {
		p.Heartbeat.TimeoutMs = *o.HeartbeatTimeoutMs
	}
	if o.IdleRecycle != nil {
		p.Recycle.IdleRecycle = *o.IdleRecycle
	}
	if o.MaxIdleMs != nil {
		p.Recycle.MaxIdleMs = *o.MaxIdleMs
	}
	if o.GracefulShutdownMs != nil {
		p.Recycle.GracefulShutdownMs = *o.GracefulShutdownMs
	}
	if o.BackgroundTasks != nil {
		p.Recycle.BackgroundTasks = *o.BackgroundTasks
	}
	if o.MemBytes != nil {
		p.Limits.MemBytes = *o.MemBytes
	}
	if o.CPUPercent != nil {
		p.Limits.CPUPercent = *o.CPUPercent
	}
	if o.RequestTimeoutMs != nil {
		p.Limits.RequestTimeoutMs = *o.RequestTimeoutMs
	}
	if o.StdoutLineBytes != nil {
		p.Limits.StdoutLineBytes = *o.StdoutLineBytes
	}
	if o.AutoRestart != nil {
		p.Resilience.AutoRestart = o.AutoRestart
	}
	if o.MaxRestarts != nil {
		p.Resilience.MaxRestarts = *o.MaxRestarts
	}
	if o.MaxRestartsWindowMs != nil {
		p.Resilience.MaxRestartsWindowMs = *o.MaxRestartsWindowMs
	}
	if o.BackoffBaseMs != nil {
		p.Resilience.BackoffBaseMs = *o.BackoffBaseMs
	}
	if o.BackoffMaxMs != nil {
		p.Resilience.BackoffMaxMs = *o.BackoffMaxMs
	}
	if o.CrashExitCodes != nil {
		p.Resilience.CrashExitCodes = o.CrashExitCodes
	}
	return p
}

// ValidateOverride 校验覆盖字段的合法性（与 manifest 校验对齐但不要求完整策略）。
func ValidateOverride(ov Override) error {
	// 先把当前覆盖包成完整策略再整体校验（借用 ApplyDefaults + Validate）。
	if ov.LoadMode != nil {
		lm := strings.TrimSpace(*ov.LoadMode)
		switch lm {
		case LoadModeAlways, LoadModePrewarm, LoadModeLazy, LoadModeDisabled:
		default:
			return fmt.Errorf("invalid load_mode %q", lm)
		}
	}
	if ov.MaxIdleMs != nil && *ov.MaxIdleMs <= 0 {
		return errors.New("max_idle_ms must be > 0")
	}
	if ov.HeartbeatIntervalMs != nil && *ov.HeartbeatIntervalMs <= 0 {
		return errors.New("heartbeat_interval_ms must be > 0")
	}
	if ov.HeartbeatTimeoutMs != nil && ov.HeartbeatIntervalMs != nil && *ov.HeartbeatTimeoutMs < *ov.HeartbeatIntervalMs {
		return fmt.Errorf("heartbeat_timeout_ms(%d) must be >= interval_ms(%d)", *ov.HeartbeatTimeoutMs, *ov.HeartbeatIntervalMs)
	}
	if ov.MemBytes != nil && *ov.MemBytes < 0 {
		return errors.New("mem_bytes must be >= 0")
	}
	if ov.CPUPercent != nil && (*ov.CPUPercent < 0 || *ov.CPUPercent > 100) {
		return errors.New("cpu_percent must be within [0,100]")
	}
	if ov.MaxRestarts != nil && *ov.MaxRestarts < 0 {
		return errors.New("max_restarts must be >= 0")
	}
	if ov.StdoutLineBytes != nil {
		const hardLimit = 8 * 1024 * 1024
		if *ov.StdoutLineBytes > hardLimit {
			return fmt.Errorf("stdout_line_bytes(%d) exceeds hard limit %d", *ov.StdoutLineBytes, hardLimit)
		}
	}
	return nil
}
