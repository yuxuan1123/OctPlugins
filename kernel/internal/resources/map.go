// Package resources 管理外部共享资源（工具 + 模型）：声明、定位、引用计数与状态登记。
//
// 对齐《理想架构.md》§14：
//   - 工具与模型本质同类：全局单例、插件共享、需引用计数（§14 引言）；
//   - 一期模型推理在插件进程内（§14.11）：宿主只负责声明/定位/引用计数/状态登记，
//     不承担加载与推理调度；
//   - 引用计数与常驻策略为 AND 关系（§14.9）：计数归零表示可以关，策略表示不想关；
//   - State 按 Kind 分开（§12.1）：禁止求并集。
package resources

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Kind 资源种类（§12.1 按 Kind 分离状态枚举）。
type Kind uint8

const (
	KindTool Kind = iota
	KindModel
)

func (k Kind) String() string {
	switch k {
	case KindTool:
		return "tool"
	case KindModel:
		return "model"
	}
	return "unknown"
}

// StopPolicy 关闭/卸载策略（§9.1 mode：resident / idleTimeout / onDemand）。
type StopPolicy struct {
	Mode        string
	IdleMinutes int
}

// Entry 一个资源的登记项（§14.4）。State 按 Kind 断言为 ProcState / ModelState 字符串。
type Entry struct {
	ID       string
	Kind     Kind
	Backend  string // KindModel 时指向 toolId（backend 工具）
	Handle   any    // *exec.Cmd（tool）或 *ModelHandle（model）
	Policy   StopPolicy
	State    string // ProcState（tool）或 ModelState（model），按 Kind 断言
	RefCount int

	LocateResult LocateResult                // kind=tool：最近一次定位结果（registry 落盘 / 复用）
	spawn        func(context.Context) error // tool：派生进程；model：就绪登记
	stop         func() error                // tool：终止进程；model：卸载（不删盘）

	mu sync.Mutex
}

// Manager ResourceMap（§14.4 sync.Map）。
type Manager struct {
	m sync.Map // map[string]*Entry
}

// NewManager 构造 ResourceMap。
func NewManager() *Manager { return &Manager{} }

// Register 登记一个资源条目（声明阶段）；重复登记返回已有条目（幂等）。
func (m *Manager) Register(e *Entry) *Entry {
	if old, ok := m.m.Load(e.ID); ok {
		return old.(*Entry)
	}
	m.m.Store(e.ID, e)
	return e
}

// Get 按 id 取条目；不存在返回 nil。
func (m *Manager) Get(id string) *Entry {
	if v, ok := m.m.Load(id); ok {
		return v.(*Entry)
	}
	return nil
}

// Unregister 摘除条目（§14.10 崩溃/显式清理时）。
func (m *Manager) Unregister(id string) { m.m.Delete(id) }

// Range 遍历全部条目并回调（调用方不得在回调内改动 map；用于 registry 落盘/退出清理）。
func (m *Manager) Range(fn func(id, kind, state string, refCount int)) {
	m.m.Range(func(k, v any) bool {
		e := v.(*Entry)
		fn(k.(string), e.Kind.String(), e.State, e.RefCount)
		return true
	})
}

// mustGet 不存在即报错（§14.5 语义）。
func (m *Manager) mustGet(id string) (*Entry, error) {
	e := m.Get(id)
	if e == nil {
		return nil, fmt.Errorf("resource %q not registered", id)
	}
	return e, nil
}

// Acquire 获取资源（§14.5）：已在运行则复用且不新建，仅递增引用计数；
// 模型先确保 backend 工具（先启动 backend，再就绪登记）。
func (m *Manager) Acquire(ctx context.Context, id string) error {
	e, err := m.mustGet(id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Kind == KindModel && e.Backend != "" {
		if err := m.Acquire(ctx, e.Backend); err != nil {
			return fmt.Errorf("backend %s: %w", e.Backend, err)
		}
		if e.State == "ready" {
			e.RefCount++
			return nil
		}
		if e.spawn == nil {
			return fmt.Errorf("model %s has no readiness hook", id)
		}
		if err := e.spawn(ctx); err != nil {
			m.Release(e.Backend) // 就绪失败：回退 backend 引用
			return err
		}
		e.State = "ready"
		e.RefCount++
		return nil
	}
	// tool：复用或派生
	if e.State == "running" {
		e.RefCount++
		return nil
	}
	if e.spawn == nil {
		return fmt.Errorf("tool %s has no spawn hook", id)
	}
	if err := e.spawn(ctx); err != nil {
		return err
	}
	e.State = "running"
	e.RefCount++
	return nil
}

// Release 释放资源（§14.5）：递减引用计数；
// 计数归零时按策略处置（onDemand 立即停止/卸载；resident 保持；idleTimeout 由巡检器处理）。
func (m *Manager) Release(id string) {
	e := m.Get(id)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.RefCount > 0 {
		e.RefCount--
	}
	if e.RefCount > 0 {
		return
	}
	// 计数归零：按策略
	if e.Policy.Mode == "resident" {
		return
	}
	if e.Policy.Mode == "onDemand" || e.Policy.Mode == "" {
		if e.stop != nil {
			_ = e.stop() // model：仅卸载，权重保留（§14.7）
		}
		if e.Kind == KindTool {
			e.State = "registered"
		} else {
			e.State = "unloading"
		}
		if e.Kind == KindModel && e.Backend != "" {
			m.Release(e.Backend) // 递减后端工具引用（§14.5 注释）
		}
	}
}

// Stop 无条件停止资源（宿主显式操作 / 退出清理时）。
func (m *Manager) Stop(id string) error {
	e, err := m.mustGet(id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stop != nil {
		return e.stop()
	}
	return nil
}

// StopAll 停止全部资源（§12.4 退出清理兜底）。
func (m *Manager) StopAll() {
	m.m.Range(func(_, v any) bool {
		e := v.(*Entry)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.stop != nil {
			_ = e.stop()
		}
		return true
	})
}

// LastActive 返回最后活跃时间（§14.9 空闲计时依据；MVP 近似为最近 Release）。
func (e *Entry) LastActive() time.Time { return time.Now() }
