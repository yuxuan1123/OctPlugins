package lifecycle

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/octplugin/kernel/internal/resources"
)

// 本文件：handover 双模型窗口（转化域 §11 / §12 / §13）。
//
// 场景：用户从已加载的快模型切到慢模型时，不让用户空等——
//   §12.1 立即用已加载的快模型开始首个任务；
//   §12.2 后台并行加载慢模型，UI 显示「正在加载 X，当前由 Y 处理」；
//   §12.3 慢模型就绪 → 后续任务切过去 → Release 快模型；
//   §12.4 慢模型失败或超时 → 快模型继续服务，降级不停工；
//   §12.5 全程对用户可见（models.handover.* 事件）。
//
// §11.1/§11.2：窗口内该能力上限临时为 2（graceUntil），超时强制回落 1。
// §13.1/§13.3：并存期显存按两者之和计，且判断在开窗前做——不要开窗后再 OOM。
// §13.2：显存不足则放弃 handover，改为串行切换（先 Release 再 Acquire）。

// DefaultHandoverWindowMs §11.1 的 10 秒 handover 窗口。
const DefaultHandoverWindowMs = 10000

// maxHandoverWindowMs 窗口硬上限（防止作者声明一个荒谬的 coldStartMs 把窗口拉到无界）。
const maxHandoverWindowMs = 300000 // 5 分钟

// handoverWindow 返回基础窗口长度（§11.1 的 10s）。
// 允许经 OCT_HANDOVER_WINDOW_MS 覆盖，便于按机器实测调整。
func handoverWindow() time.Duration {
	if v := os.Getenv("OCT_HANDOVER_WINDOW_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return DefaultHandoverWindowMs * time.Millisecond
}

// handoverWindowFor 返回本次接班实际使用的窗口长度。
//
// 为什么不是死守 10s：§11.1 定 10s 的**目的**是「给并存一个有限窗口」，而 §12 要求
// 「慢模型接班」。但作者声明的 coldStartMs 可能远大于 10s（本机 MOSS-TTS 与 hy_mt 都是
// 15000ms），死守 10s 会让这些模型的接班**必然**超时，§12 对它们形同虚设。
// 故取 max(基础窗口, coldStartMs + 余量)：既保留 10s 作为下限（快模型仍只需 10s），
// 又让慢模型有真实的接班机会。超出基础窗口时会记一条日志，保证行为可见而非静默。
func handoverWindowFor(coldStartMs int) time.Duration {
	base := handoverWindow()
	if coldStartMs <= 0 {
		return base
	}
	need := time.Duration(coldStartMs)*time.Millisecond + 5*time.Second
	if need <= base {
		return base
	}
	if need > maxHandoverWindowMs*time.Millisecond {
		need = maxHandoverWindowMs * time.Millisecond
	}
	log.Printf("[handover] widening window %s → %s to cover declared coldStartMs=%d",
		base, need, coldStartMs)
	return need
}

// vramBudgetGB 返回宿主可用的显存预算（GB）。
//
// 《理想架构.md》§15.5 **MUST NOT** 猜测：本实现不做显存探测，只在宿主/用户
// 显式给出 OCT_VRAM_GB 时返回已知值；否则报告 unknown，调用方据此放弃开窗。
// 全部 minVramGB=0（纯 CPU EP，如 RapidOCR）的模型组合不受此限制。
func vramBudgetGB() (float64, bool) {
	v := os.Getenv("OCT_VRAM_GB")
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// HandoverState 一次接班的可见状态（§12.5）。Phase ∈ ready | loading | failed。
type HandoverState struct {
	Capability string    `json:"capability"`
	From       string    `json:"from,omitempty"` // 当前在服务的（快）模型
	To         string    `json:"to"`             // 接班的（慢）模型
	Phase      string    `json:"phase"`
	Mode       string    `json:"mode"`             // window（并行接班）| serial（串行切换）| noop
	Reason     string    `json:"reason,omitempty"` // 降级/失败原因（面向用户）
	NeedVramGB float64   `json:"needVramGB,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	Deadline   time.Time `json:"deadline,omitempty"`
}

// SwitchModel 执行一次模型接班（§12）。窗口模式下立即返回（Phase=loading），
// 完成结果经 models.handover.ready / models.handover.failed 事件广播。
//
// 返回的 HandoverState 描述「此刻」的状态：
//   - 已是目标模型 → phase=ready；
//   - 串行切换（显存不足/显存未知）→ 同步完成后返回 phase=ready 或 failed；
//   - 并行接班 → 立即返回 phase=loading，后台继续。
func (m *Manager) SwitchModel(ctx context.Context, capability, targetID string) (HandoverState, error) {
	return m.switchModel(ctx, capability, targetID, false)
}

// SwitchModelSync 与 SwitchModel 相同，但窗口模式也等待完成（供测试与紧耦合调用方）。
func (m *Manager) SwitchModelSync(ctx context.Context, capability, targetID string) (HandoverState, error) {
	return m.switchModel(ctx, capability, targetID, true)
}

func (m *Manager) switchModel(ctx context.Context, capability, targetID string, wait bool) (HandoverState, error) {
	rm := m.resourceManager()
	st := HandoverState{Capability: capability, To: targetID, StartedAt: time.Now()}
	if rm == nil {
		return st, fmt.Errorf("resource map unavailable")
	}
	if targetID == "" {
		return st, fmt.Errorf("target model id required")
	}
	if rm.Get(targetID) == nil {
		return st, fmt.Errorf("model %q not registered", targetID)
	}
	if got := rm.CapabilityOf(targetID); capability != "" && got != "" && got != capability {
		return st, fmt.Errorf("model %q provides capability %q, not %q", targetID, got, capability)
	}
	if capability == "" {
		capability = rm.CapabilityOf(targetID)
		st.Capability = capability
	}

	// 目标模型已就绪 → 无需接班。
	if e := rm.Get(targetID); e != nil && e.State == "ready" {
		st.Mode = "noop"
		st.Phase = "ready"
		return st, nil
	}

	from := m.activeModelOf(rm, capability, targetID)
	st.From = from
	if from == targetID {
		st.Phase = "ready"
		st.Mode = "noop"
		return st, nil
	}

	// 没有在服务的快模型 → 无接班对象，不需要开窗（§12 的前提是「快模型顶上」）。
	// 直接同步加载目标，语义等价于 §10.2 的先 Release 再 Acquire（此处无 from 可释放）。
	if from == "" {
		st.Mode = "serial"
		st.Reason = "该能力当前没有已加载的模型，直接加载目标模型"
		if err := rm.Acquire(ctx, targetID); err != nil {
			st.Phase = "failed"
			st.Reason = fmt.Sprintf("加载 %s 失败：%v", targetID, err)
			m.emitModelEvent("models.handover.failed", st)
			return st, nil
		}
		st.Phase = "ready"
		m.emitModelEvent("models.handover.ready", st)
		return st, nil
	}

	// ── §13.3：资源账在开窗前算 ────────────────────────────────
	need := m.modelVramGB(from) + m.modelVramGB(targetID)
	st.NeedVramGB = need
	openWindow, reason := true, ""
	if need > 0 {
		budget, known := vramBudgetGB()
		switch {
		case !known:
			openWindow = false
			reason = fmt.Sprintf("并存期需 %.1fGB 显存且可用显存未知（未设 OCT_VRAM_GB）；"+
				"按 §15.5 MUST NOT 猜测，放弃 handover 改串行切换", need)
		case need > budget:
			openWindow = false
			reason = fmt.Sprintf("并存期需 %.1fGB > 可用 %.1fGB（§13.1）", need, budget)
		}
	}
	if !openWindow {
		return m.serialSwitch(ctx, rm, st, from, targetID, reason)
	}

	// ── §11/§12：并行接班 ──────────────────────────────────────
	// 窗口按目标模型声明的 coldStartMs 自适应（见 handoverWindowFor 注释）。
	coldStart := m.modelColdStartMs(targetID)
	window := handoverWindowFor(coldStart)
	until := rm.OpenGrace(capability, window)
	m.mirrorGrace(rm, capability, until)
	st.Mode = "window"
	st.Phase = "loading"
	st.Deadline = until
	m.emitModelEvent("models.handover.started", st)

	run := func() HandoverState {
		bg, cancel := context.WithTimeout(context.Background(), window)
		defer cancel()
		err := rm.Acquire(bg, targetID)
		rm.CloseGrace(capability)
		m.mirrorGrace(rm, capability, time.Time{})
		if err != nil {
			// §12.4：慢模型失败/超时 → 快模型继续服务，降级不停工。
			fin := st
			fin.Phase = "failed"
			fin.Reason = fmt.Sprintf("加载 %s 失败或超时（%s）：%v；继续由 %s 服务", targetID, window, err, from)
			log.Printf("[handover] %s", fin.Reason)
			m.emitModelEvent("models.handover.failed", fin)
			return fin
		}
		// §12.3：就绪后切过去，并 Release 快模型。
		if from != "" {
			m.releaseFully(rm, from)
		}
		fin := st
		fin.Phase = "ready"
		fin.Reason = ""
		m.emitModelEvent("models.handover.ready", fin)
		return fin
	}

	if wait {
		return run(), nil
	}
	go run()
	return st, nil
}

// serialSwitch §13.2：放弃 handover，先 Release 快模型再 Acquire 目标模型。
func (m *Manager) serialSwitch(ctx context.Context, rm *resources.Manager, st HandoverState,
	from, targetID, reason string) (HandoverState, error) {

	st.Mode = "serial"
	st.Reason = reason
	if from != "" {
		m.releaseFully(rm, from)
	}
	if err := rm.Acquire(ctx, targetID); err != nil {
		st.Phase = "failed"
		st.Reason = fmt.Sprintf("%s；串行切换加载 %s 失败：%v", reason, targetID, err)
		m.emitModelEvent("models.handover.failed", st)
		return st, nil
	}
	st.Phase = "ready"
	m.emitModelEvent("models.handover.ready", st)
	return st, nil
}

// activeModelOf 返回某能力当前正在服务的模型 id（排除 exclude）。
// §10.1 上限默认为 1，故通常至多一个 ready 条目。
func (m *Manager) activeModelOf(rm *resources.Manager, capability, exclude string) string {
	if capability == "" {
		return ""
	}
	found := ""
	rm.Range(func(id, kind, state string, _ int) {
		if found != "" || kind != "model" || id == exclude {
			return
		}
		if e := rm.Get(id); e != nil && e.Capability == capability && state == "ready" {
			found = id
		}
	})
	return found
}

// releaseFully 把某模型的引用计数一次降到 0，真正触发卸载（§11.3 接班后 Release 快模型）。
// 引用计数由多次 Acquire 累积，故循环释放直到模型不再处于 ready。
func (m *Manager) releaseFully(rm *resources.Manager, id string) {
	for i := 0; i < 8; i++ {
		e := rm.Get(id)
		if e == nil || e.State != "ready" || e.RefCount <= 0 {
			return
		}
		rm.Release(id)
	}
}

// mirrorGrace 把窗口截止时刻镜像到 Entry.GraceUntil（§11.2 要求的字段），供外部查看。
func (m *Manager) mirrorGrace(rm *resources.Manager, capability string, until time.Time) {
	rm.Range(func(id, kind, _ string, _ int) {
		if kind != "model" {
			return
		}
		if e := rm.Get(id); e != nil && e.Capability == capability {
			e.SetGraceUntil(until)
		}
	})
}

// modelVramGB 取某模型声明的显存占用（未登记/未声明返回 0）。
func (m *Manager) modelVramGB(id string) float64 {
	if id == "" {
		return 0
	}
	me, ok := m.modelByID(id)
	if !ok {
		return 0
	}
	return me.MinVramGB
}

// modelColdStartMs 取某模型声明的冷启动耗时（未登记返回 0）。
func (m *Manager) modelColdStartMs(id string) int {
	me, ok := m.modelByID(id)
	if !ok {
		return 0
	}
	return me.ColdStartMs
}
