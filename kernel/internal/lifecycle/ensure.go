package lifecycle

import (
	"context"
	"fmt"
	"time"
)

// 本文件：插件→宿主「确保某能力/某模型就绪」的唯一入口（转化域 §22.3 / §四）。
//
// conversion 插件不写具体 model id（§23.1 只声明 requiresCapabilities），
// 也不得自行加载模型；它只调用内核的 gate.model_ensure，由宿主：
//   1) 按 §15.4 优先级链解析该能力的生效模型；
//   2) 若该模型尚未就绪，按 §12 走 handover —— 立即用已加载的快模型开始首个任务，
//      后台并行加载慢模型，UI 经事件看到「正在加载 X，当前由 Y 处理」。

// EnsureResult 一次「确保模型就绪」的结果。
type EnsureResult struct {
	Capability string `json:"capability"`
	ModelID    string `json:"modelId,omitempty"` // 本次任务应当使用的模型（可能是仍在服务的快模型）
	Source     string `json:"source,omitempty"`  // §15.4 解析来源：userPin/authorDefault/fallback/hostFallback
	State      string `json:"state"`             // ready | loading | none
	Pending    string `json:"pending,omitempty"` // 正在后台接班的模型（§12.2）
	Reason     string `json:"reason,omitempty"`  // 降级/兜底原因（§8.3 写入结果元数据）
	Note       string `json:"note,omitempty"`    // 面向用户的说明（§12.2 文案）
}

// SetPinsProvider 注入用户能力级 pin 的读取器（由装配根接 user-settings.json）。
// 放在 provider 里而非直接依赖 config 包，避免 config → lifecycle → config 的循环。
func (m *Manager) SetPinsProvider(fn func() map[string]string) {
	m.mu.Lock()
	m.pinsFn = fn
	m.mu.Unlock()
}

// pins 读取当前用户 pin（未注入返回空表）。
func (m *Manager) pins() map[string]string {
	m.mu.Lock()
	fn := m.pinsFn
	m.mu.Unlock()
	if fn == nil {
		return map[string]string{}
	}
	return fn()
}

// SetPinWriter 注入能力级 pin 的写入器（由装配根接到 config.Settings.SetPin）。
// 与 SetPinsProvider 同理：lifecycle 不直接依赖 config，避免 config→lifecycle→config 循环。
func (m *Manager) SetPinWriter(fn func(capability, modelID string) error) {
	m.mu.Lock()
	m.pinWriteFn = fn
	m.mu.Unlock()
}

// SetPin 写入/清除某能力的用户 pin（§8.1 设置级持久默认）。
// modelID 为空表示清除 pin、回落作者声明。未注入写入器时返回错误（不静默失败）。
func (m *Manager) SetPin(capability, modelID string) error {
	m.mu.Lock()
	fn := m.pinWriteFn
	m.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("pin store unavailable")
	}
	return fn(capability, modelID)
}

// 插件「自己的」设置（plugins[id].settings）经注入函数读写，避免 lifecycle→config 循环导入
// （config 已依赖 lifecycle）。§17.2 A：设置由宿主「设置→插件/tool内部设置」页按 settingsSchema
// 渲染编辑；插件只读自己的生效设置、只写 schema 声明字段，不直读 config（§17.3）。

// SetSettingsResolver 注入插件自身生效设置的读取器（装配根接 config.Settings + config.ResolvePlugin）。
func (m *Manager) SetSettingsResolver(fn func(pluginID string, mf Manifest) (map[string]any, error)) {
	m.mu.Lock()
	m.settingsGetFn = fn
	m.mu.Unlock()
}

// SetSettingsWriter 注入插件自身设置的写入器（装配根接 config.Settings，仅写 schema 声明字段）。
func (m *Manager) SetSettingsWriter(fn func(pluginID string, mf Manifest, settings map[string]any) error) {
	m.mu.Lock()
	m.settingsSetFn = fn
	m.mu.Unlock()
}

// SettingsEffective 返回某插件的生效设置（user-settings plugins[id].settings 经 manifest 默认值合并）。
// 未注入读取器时返回空表（不阻塞插件）。
func (m *Manager) SettingsEffective(id string, mf Manifest) (map[string]any, error) {
	m.mu.Lock()
	fn := m.settingsGetFn
	m.mu.Unlock()
	if fn == nil {
		return map[string]any{}, nil
	}
	return fn(id, mf)
}

// SetSettings 写回某插件的设置（仅 schema 声明字段）；未注入写入器时返回错误（不静默失败）。
func (m *Manager) SetSettings(id string, mf Manifest, settings map[string]any) error {
	m.mu.Lock()
	fn := m.settingsSetFn
	m.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("settings store unavailable")
	}
	return fn(id, mf, settings)
}

// EnsureModel 解析并确保「某能力（可指定 override 模型）」就绪，返回本次任务应使用的模型。
//
// overrideID 非空即 §8.2 的任务级 override（一次性，不改全局 pin）。
func (m *Manager) EnsureModel(ctx context.Context, capability, overrideID string) (EnsureResult, error) {
	res := EnsureResult{Capability: capability, State: "none"}
	rm := m.resourceManager()
	if rm == nil {
		return res, fmt.Errorf("resource map unavailable")
	}
	if capability == "" && overrideID != "" {
		capability = m.CapabilityOf(overrideID)
		res.Capability = capability
	}
	if capability == "" {
		return res, fmt.Errorf("capability or modelId required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	target := overrideID
	if target == "" {
		// §15.4 优先级链：用户 pin > 作者默认 > 量化变体 > fallbacks > 宿主兜底
		r := m.ResolveCapability(capability, m.pins())
		res.Source = r.Source
		res.Reason = r.Reason
		target = r.ModelID
	} else {
		res.Source = "taskOverride"
	}
	if target == "" {
		res.State = "none"
		return res, fmt.Errorf("no usable model for capability %q: %s", capability, res.Reason)
	}

	// 已就绪 → 零等待（§7.4）。
	if e := rm.Get(target); e != nil && e.State == "ready" {
		res.ModelID, res.State = target, "ready"
		return res, nil
	}

	current := m.activeModelOf(rm, capability, target)
	if current == "" {
		// 没有在服务的快模型：直接加载目标（无可接班对象，不涉及窗口）。
		if err := rm.Acquire(ctx, target); err != nil {
			res.State = "none"
			return res, err
		}
		res.ModelID, res.State = target, "ready"
		return res, nil
	}

	// §12：已有快模型在服务 → 开窗并行加载目标，首个任务继续用快模型。
	st, err := m.SwitchModel(ctx, capability, target)
	if err != nil {
		return res, err
	}
	res.ModelID = current
	switch st.Phase {
	case "ready":
		res.ModelID, res.State = target, "ready"
	case "failed":
		res.State = "ready"
		res.Pending = ""
		res.Note = st.Reason
	default: // loading
		res.Pending = target
		// §12.3 场景 C：用户 pin / 任务级 override 的模型是「指定目标」，不能像作者默认
		// 的 fast 档一样用快模型顶上就算了——否则插件侧（oct_sdk acquire / gateway
		// registry.call）拿到 loading 状态会直接 E_MODEL_NOT_READY，pin 永不生效。
		// 故在 handover 窗口内等待目标就绪再返回；窗口超时才退回快模型（降级不停工）。
		if res.Source == "userPin" || res.Source == "taskOverride" {
			until := handoverWindowFor(m.modelColdStartMs(target))
			if m.waitTargetReady(ctx, target, until) {
				res.ModelID, res.State = target, "ready"
				res.Pending = ""
				res.Note = ""
			} else {
				res.ModelID, res.State = current, "ready"
				res.Pending = ""
				res.Note = fmt.Sprintf("指定模型 %s 未在 %s 内就绪，本次退回 %s", target, until, current)
			}
			break
		}
		res.ModelID = current
		res.State = "loading"
		res.Note = fmt.Sprintf("当前由 %s 处理，正在加载 %s", current, target)
	}
	return res, nil
}

// waitTargetReady 在 handover 窗口内轮询目标模型就绪（§12.3 场景 C 等待接班）。
// ready → true；failed → false；窗口超时 / ctx 取消 → false。0.5s 采样一次，
// 不阻塞 ping 等关键路径（模型由 SwitchModel 的后台 run() 加载）。
func (m *Manager) waitTargetReady(ctx context.Context, target string, until time.Duration) bool {
	deadline := time.Now().Add(until)
	for {
		if e := m.resourceManager().Get(target); e != nil {
			switch e.State {
			case "ready":
				return true
			case "failed":
				return false
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// EnsureModelDirect 由宿主（非插件）确保某模型 id 就绪，不做 §15.4 解析。
// 供 registry.models.acquire 的语义增强使用：已就绪直接返回，否则按 §12 接班。
func (m *Manager) EnsureModelDirect(ctx context.Context, modelID string) (EnsureResult, error) {
	res := EnsureResult{State: "none"}
	rm := m.resourceManager()
	if rm == nil {
		return res, fmt.Errorf("resource map unavailable")
	}
	if modelID == "" {
		return res, fmt.Errorf("modelId required")
	}
	res.ModelID = modelID
	res.Capability = rm.CapabilityOf(modelID)
	if e := rm.Get(modelID); e != nil && e.State == "ready" {
		res.State = "ready"
		return res, nil
	}
	cur := m.activeModelOf(rm, res.Capability, modelID)
	if cur == "" {
		if err := rm.Acquire(ctx, modelID); err != nil {
			return res, err
		}
		res.State = "ready"
		return res, nil
	}
	st, err := m.SwitchModel(ctx, res.Capability, modelID)
	if err != nil {
		return res, err
	}
	if st.Phase == "loading" {
		res.ModelID, res.State, res.Pending = cur, "loading", modelID
		res.Note = fmt.Sprintf("当前由 %s 处理，正在加载 %s", cur, modelID)
		return res, nil
	}
	res.State = "ready"
	res.ModelID = modelID
	return res, nil
}

// ensureTimeout 是同步路径上「确保就绪」的兜底上限（不含 handover 窗口）。
const ensureTimeout = 120 * time.Second
