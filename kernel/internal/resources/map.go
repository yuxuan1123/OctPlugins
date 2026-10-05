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
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// ErrCapabilityBusy 该能力维度已达并发上限（§10.1；§11.2 窗口内上限临时 +1）。
// 调用方应改为「先 Release 再 Acquire」（§10.2 换模型语义），或走 handover 流程。
var ErrCapabilityBusy = errors.New("capability already has an active model")

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

	// Capability 该模型提供的能力维度（§10.1）。仅对显式声明了 provides 的模型非空；
	// 为空者按普通资源 id 引用计数，不受能力维度上限约束。
	Capability string

	// Companion 标记伴随模型（如 RapidOCR 的 det+rec 对、MOSS-TTS 的 codec 依赖）：
	// 与主模型构成一个模型组，随主模型一起加载，不计入 §10.1 的能力维度上限。
	Companion bool

	// GraceUntil handover 窗口截止时刻（§11.2）：非零且未过期时，该能力上限临时为 2，
	// 用于「快模型顶上、慢模型接班」。超时后由 Release/巡检强制回落 1。
	GraceUntil time.Time

	LocateResult LocateResult                // kind=tool：最近一次定位结果（registry 落盘 / 复用）
	spawn        func(context.Context) error // tool：派生进程；model：就绪登记
	stop         func() error                // tool：终止进程；model：卸载（不删盘）

	mu sync.Mutex
}

// Manager ResourceMap（§14.4 sync.Map）。
type Manager struct {
	m sync.Map // map[string]*Entry

	mu sync.Mutex
	// capLimit 能力维度上限（§10.1），默认 1；仅对 Capability 非空的模型生效。
	capLimit map[string]int
	// capActive 已就绪的模型数（按能力维度）。
	capActive map[string]int
	// capReserved 正在加载中的预留（避免 spawn 期间并发穿透上限）。
	capReserved map[string]int
	// grace 能力维度 → handover 窗口截止时刻（§11.2）。存于 Manager 侧以避免
	// 取 m.mu 的同时再取 Entry.mu 造成锁序反转；Entry.GraceUntil 同步镜像供外部查看。
	grace map[string]time.Time
}

// NewManager 构造 ResourceMap。
func NewManager() *Manager {
	return &Manager{
		capLimit:    map[string]int{},
		capActive:   map[string]int{},
		capReserved: map[string]int{},
		grace:       map[string]time.Time{},
	}
}

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
func (m *Manager) Unregister(id string) {
	if e := m.Get(id); e != nil {
		m.releaseCapSlot(e)
	}
	m.m.Delete(id)
}

// Range 遍历全部条目并回调（调用方不得在回调内改动 map；用于 registry 落盘/退出清理）。
func (m *Manager) Range(fn func(id, kind, state string, refCount int)) {
	m.m.Range(func(k, v any) bool {
		e := v.(*Entry)
		fn(k.(string), e.Kind.String(), e.State, e.RefCount)
		return true
	})
}

// ── §10.1 能力维度上限 ───────────────────────────────────────────

// SetCapabilityLimit 设置某能力的并发上限（默认 1）。limit<=0 视为 1。
func (m *Manager) SetCapabilityLimit(capability string, limit int) {
	if capability == "" {
		return
	}
	if limit <= 0 {
		limit = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.capLimit[capability] = limit
}

// OpenGrace 为某能力开启 handover 窗口（§11.2）：窗口内该能力上限临时 +1。
func (m *Manager) OpenGrace(capability string, d time.Duration) time.Time {
	until := time.Now().Add(d)
	m.mu.Lock()
	m.grace[capability] = until
	m.mu.Unlock()
	return until
}

// CloseGrace 关闭某能力的 handover 窗口并强制回落上限（§11.2 超时强制回落 1）。
func (m *Manager) CloseGrace(capability string) {
	m.mu.Lock()
	delete(m.grace, capability)
	m.mu.Unlock()
	// 同步清掉镜像字段，避免外部看到过期窗口。
	m.m.Range(func(_, v any) bool {
		e := v.(*Entry)
		if e.Kind == KindModel && e.Capability == capability {
			e.mu.Lock()
			e.GraceUntil = time.Time{}
			e.mu.Unlock()
		}
		return true
	})
}

// InGrace 报告某能力是否处于 handover 窗口内（过期即自动失效）。
func (m *Manager) InGrace(capability string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inGraceLocked(capability)
}

func (m *Manager) inGraceLocked(capability string) bool {
	until, ok := m.grace[capability]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(m.grace, capability) // 超时强制回落
		return false
	}
	return true
}

// capLimitLocked 返回某能力的当前有效上限（基础值，窗口内 +1）。
func (m *Manager) capLimitLocked(capability string) int {
	base := m.capLimit[capability]
	if base <= 0 {
		base = 1
	}
	if m.inGraceLocked(capability) {
		base++
	}
	return base
}

// CapabilityUsage 报告某能力当前已就绪模型数与有效上限（供状态视图/资源账）。
func (m *Manager) CapabilityUsage(capability string) (active, limit int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capActive[capability], m.capLimitLocked(capability)
}

// CapabilityOf 报告某模型的所属能力维度（工具/未声明 capability 的模型返回空）。
func (m *Manager) CapabilityOf(id string) string {
	e := m.Get(id)
	if e == nil || e.Kind != KindModel {
		return ""
	}
	return e.Capability
}

// reserveCapSlot 为一次模型加载预留能力槽位；超限返回 ErrCapabilityBusy。
func (m *Manager) reserveCapSlot(capability string) error {
	if capability == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := m.capLimitLocked(capability)
	used := m.capActive[capability] + m.capReserved[capability]
	if used >= limit {
		return fmt.Errorf("%w: %q has %d active model(s), limit %d",
			ErrCapabilityBusy, capability, used, limit)
	}
	m.capReserved[capability]++
	return nil
}

// commitCapSlot 预留转正（模型已就绪）。
func (m *Manager) commitCapSlot(capability string) {
	if capability == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.capReserved[capability] > 0 {
		m.capReserved[capability]--
	}
	m.capActive[capability]++
}

// dropCapReservation 加载失败时归还预留。
func (m *Manager) dropCapReservation(capability string) {
	if capability == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.capReserved[capability] > 0 {
		m.capReserved[capability]--
	}
}

// capDimension 返回该条目参与 §10.1 上限计算的能力维度。
// 伴随模型（det+rec / codec 依赖）返回空，按普通资源 id 引用计数。
func (e *Entry) capDimension() string {
	if e.Kind != KindModel || e.Companion {
		return ""
	}
	return e.Capability
}

// releaseCapSlot 模型卸载时归还活跃槽位。
func (m *Manager) releaseCapSlot(e *Entry) {
	cap := e.capDimension()
	if cap == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.capActive[cap] > 0 {
		m.capActive[cap]--
	}
}

// SnapshotCapabilities 返回全部被登记过的能力维度及其用量（供 registry.capabilities.list）。
func (m *Manager) SnapshotCapabilities() map[string][2]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][2]int{}
	add := func(cap string) {
		if cap == "" {
			return
		}
		if _, seen := out[cap]; !seen {
			out[cap] = [2]int{m.capActive[cap], m.capLimitLocked(cap)}
		}
	}
	for cap := range m.capLimit {
		add(cap)
	}
	for cap := range m.capActive {
		add(cap)
	}
	m.m.Range(func(_, v any) bool {
		e := v.(*Entry)
		if e.Kind == KindModel {
			add(e.capDimension())
		}
		return true
	})
	return out
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
//
// 转化域 §四：模型的加载经 ModelHooks.Ensure 由宿主发起，tool 只执行、不自行触发。
// §10.1：模型按其 capability 维度受并发上限约束（默认 1；§11.2 窗口内临时 2）。
func (m *Manager) Acquire(ctx context.Context, id string) error {
	e, err := m.mustGet(id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Kind == KindModel && e.Backend != "" {
		// §14.11：宿主只做就绪登记、不承担推理调度；
		// backend 工具定位/启动失败仅降级（记录日志），不阻塞模型就绪登记——
		// 否则 onnxruntime 这类「库资源」无 PATH 可执行名时模型永远无法 Acquire。
		if err := m.Acquire(ctx, e.Backend); err != nil {
			log.Printf("[resources] model %s backend %s acquire degraded: %v", id, e.Backend, err)
		}
		if e.State == "ready" {
			e.RefCount++
			return nil
		}
		if e.spawn == nil {
			return fmt.Errorf("model %s has no readiness hook", id)
		}
		// §10.1 能力维度上限（仅对声明了 capability 且非伴随的模型生效）。
		dim := e.capDimension()
		if err := m.reserveCapSlot(dim); err != nil {
			return err
		}
		if err := e.spawn(ctx); err != nil {
			m.dropCapReservation(dim)
			e.State = "failed"
			return err
		}
		m.commitCapSlot(dim)
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
			m.releaseCapSlot(e) // §10.1：归还该能力维度的活跃槽位
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
