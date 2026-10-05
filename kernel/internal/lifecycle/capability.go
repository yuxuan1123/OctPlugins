package lifecycle

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/octplugin/kernel/internal/registry"
)

// 本文件：能力维度的模型解析（转化域 §7 / §8 / §9）。
//
// 《理想架构.md》§15.4 的解析优先级，按转化域 §9.2 修订为：
//
//	用户 pin（显式覆盖）
//	  > 作者声明的模型（default:true）
//	    > 量化变体（宿主按显存选择）
//	      > 作者声明的 fallbacks（显存不足/首选不可用时）
//	        > 宿主能力兜底（同能力候选中最快者，须告知用户）
//
// §9.3：若把用户 pin 放在最末尾，hy_mt / opusMT 这类同能力多模型的选择将永远不可达，
// 故 MUST 置顶。

// DefaultModelsRoot 模型存储的内置默认根（转化域 §20.5）。
// 用户可在 user-settings.json 的 models.root 覆盖。
const DefaultModelsRoot = `D:\AI_Model`

// modelRec 是 registry.ModelEntry 加上它的 map key（registry 里 id 是键，不在条目内）。
type modelRec struct {
	ID string
	registry.ModelEntry
}

// ModelCandidate 一个能力候选模型的解析视图。
type ModelCandidate struct {
	ID          string   `json:"id"`
	Provider    string   `json:"provider,omitempty"`
	Backend     string   `json:"backend,omitempty"`
	Quant       string   `json:"quant,omitempty"`
	Path        string   `json:"path,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	SizeBytes   int64    `json:"sizeBytes,omitempty"`
	MinVramGB   float64  `json:"minVramGB,omitempty"`
	License     string   `json:"license,omitempty"`
	QualityTier string   `json:"qualityTier,omitempty"`
	ColdStartMs int      `json:"coldStartMs,omitempty"`
	Default     bool     `json:"default,omitempty"`
	Fallbacks   []string `json:"fallbacks,omitempty"`
	Companion   bool     `json:"companion,omitempty"`
	DownloadURL string   `json:"downloadUrl,omitempty"` // 非空 = 宿主可代为下载（§26）
	State       string   `json:"state,omitempty"`
	RefCount    int      `json:"refCount,omitempty"`
	Pinned    bool `json:"pinned"`
	Effective bool `json:"effective"`
	// Unavailable 空串表示可用；非空为不可用原因（§25：不可用项灰显并说明原因）。
	// 不用 omitempty：契约要求该字段恒为字符串，便于 UI 直接判断与展示。
	Unavailable string `json:"unavailable"`
}

// ModelResolution 一个能力的生效模型与来源（§8.3 写入结果元数据）。
type ModelResolution struct {
	Capability string `json:"capability"`
	ModelID    string `json:"modelId,omitempty"`
	Source     string `json:"source"` // userPin | authorDefault | quantVariant | fallback | hostFallback | none
	Reason     string `json:"reason"`
	// Pinned 用户 pin 值；未 pin 时为空串（不用 omitempty，UI 契约要求恒为字符串）。
	Pinned     string           `json:"pinned"`
	Candidates []ModelCandidate `json:"candidates,omitempty"`
}

// CapabilityOf 返回某模型提供的能力维度；非模型或未声明 capability 时返回空。
func (m *Manager) CapabilityOf(modelID string) string {
	if rm := m.resourceManager(); rm != nil {
		if c := rm.CapabilityOf(modelID); c != "" {
			return c
		}
	}
	if me, ok := m.modelByID(modelID); ok {
		return me.Capability
	}
	return ""
}

// ModelsRootDir 返回本次运行使用的模型存储根：用户配置优先，否则内置默认。
func (m *Manager) ModelsRootDir() string {
	m.mu.Lock()
	root := m.modelsRoot
	m.mu.Unlock()
	if strings.TrimSpace(root) == "" {
		return DefaultModelsRoot
	}
	return root
}

// SetModelsRoot 设置模型存储根（§20.5：由用户决定模型放哪）。
func (m *Manager) SetModelsRoot(dir string) {
	m.mu.Lock()
	m.modelsRoot = strings.TrimSpace(dir)
	m.mu.Unlock()
}

// ResolveModelPath 把模型声明的 Path 解析成绝对路径（§20.5）。
//   - 已是绝对路径：原样返回（兼容既有的 D:\AI_Model\<provider>\... 布局）；
//   - 相对路径：相对本次运行的模型根解析，形如 <modelsRoot>/<modelId>/<quant>/。
func (m *Manager) ResolveModelPath(declared string) string {
	p := strings.TrimSpace(declared)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(m.ModelsRootDir(), filepath.FromSlash(p))
}

// modelAvailability 判定某模型是否可用，返回空串表示可用，否则返回原因（§25）。
func (m *Manager) modelAvailability(me registry.ModelEntry) string {
	if me.Provider == "" {
		return "无 provider 声明"
	}
	p := m.ResolveModelPath(me.Path)
	if p == "" {
		// 未声明路径的模型（外部 outtool 自管的，如 ollama）：不做在位性判定。
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		return fmt.Sprintf("模型文件缺失：%s", p)
	}
	return ""
}

// modelsByCapability 返回某能力的全部候选（按 id 排序，保证结果稳定）。
// 伴随模型（det+rec / codec 依赖）不是该能力的独立候选，故被排除——它随主模型一起加载。
func (m *Manager) modelsByCapability(capability string) []modelRec {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil {
		return nil
	}
	f, err := rs.Load()
	if err != nil {
		log.Printf("[models] load registry: %v", err)
		return nil
	}
	var out []modelRec
	for id, me := range f.Models {
		if me.Capability != capability || me.Companion {
			continue
		}
		out = append(out, modelRec{ID: id, ModelEntry: me})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// fastest 在候选中挑「最快」：先冷启动，再体积，最后 id（确定性）。
func fastest(cands []modelRec) (modelRec, bool) {
	if len(cands) == 0 {
		return modelRec{}, false
	}
	best := cands[0]
	for _, c := range cands[1:] {
		switch {
		case c.ColdStartMs > 0 && (best.ColdStartMs == 0 || c.ColdStartMs < best.ColdStartMs):
			best = c
		case c.ColdStartMs == best.ColdStartMs && c.SizeBytes > 0 && (best.SizeBytes == 0 || c.SizeBytes < best.SizeBytes):
			best = c
		}
	}
	return best, true
}

// pickQuantVariant 执行 §15.5 的量化自动适配：同能力内按可用显存挑量化档位。
//
// 当前登记里没有任何模型声明多个量化变体（每个 id 只有一个 quant），
// 故这一步是按 §15.5 表实现的、当前为直通的真实钩子——一旦作者登记 q4/q5/q8 变体即生效。
func (m *Manager) pickQuantVariant(base modelRec, family []modelRec) (modelRec, string) {
	if len(family) <= 1 {
		return base, ""
	}
	budget, known := vramBudgetGB()
	if !known {
		// §15.5 MUST NOT 猜测：显存未知时保持作者/宿主已选定的档位。
		return base, ""
	}
	want := "q4_k_m"
	switch {
	case budget >= 24:
		want = "fp16"
	case budget >= 12:
		want = "q5_k_m"
	}
	for _, c := range family {
		if strings.EqualFold(c.Quant, want) {
			if c.ID == base.ID {
				return base, ""
			}
			return c, fmt.Sprintf("按显存 %.0fGB 选择量化变体 %s（§15.5）", budget, want)
		}
	}
	return base, ""
}

// ResolveCapability 按修订后的 §15.4 优先级链解析某能力的生效模型。
// pins 为用户能力级 pin（capability → modelId），来自 user-settings.json。
func (m *Manager) ResolveCapability(capability string, pins map[string]string) ModelResolution {
	res := m.resolveCapability(capability, pins)
	markEffective(&res)
	return res
}

func (m *Manager) resolveCapability(capability string, pins map[string]string) ModelResolution {
	res := ModelResolution{Capability: capability}
	cands := m.modelsByCapability(capability)
	res.Candidates = m.candidateViews(cands)

	if len(cands) == 0 {
		res.Source = "none"
		res.Reason = fmt.Sprintf("没有任何 tool 声明提供 %s 能力的模型", capability)
		return res
	}
	byID := map[string]modelRec{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	avail := func(me registry.ModelEntry) string { return m.modelAvailability(me) }

	// ① 用户 pin（§9.2 置顶；§8.1 设置级持久默认）
	pin := strings.TrimSpace(pins[capability])
	if pin != "" {
		res.Pinned = pin
		if me, ok := byID[pin]; ok {
			why := avail(me.ModelEntry)
			if why == "" {
				res.ModelID, res.Source = me.ID, "userPin"
				return res
			}
			res.Reason = fmt.Sprintf("用户 pin 的 %s 不可用（%s），降级到作者声明", pin, why)
		} else {
			res.Reason = fmt.Sprintf("用户 pin 的 %s 未登记，降级到作者声明", pin)
		}
	}

	// ② 作者声明的默认模型（default:true）
	var declared []modelRec
	for _, c := range cands {
		if c.Default {
			declared = append(declared, c)
		}
	}
	if len(declared) > 1 {
		log.Printf("[models] capability %s declares %d defaults; picking the first available (author SHOULD mark only one)",
			capability, len(declared))
	}
	for _, d := range declared {
		if why := avail(d.ModelEntry); why == "" {
			// ③ 量化变体（§15.5）
			refined, note := m.pickQuantVariant(d, cands)
			res.ModelID = refined.ID
			res.Source = "authorDefault"
			if note != "" {
				res.Source = "quantVariant"
				res.Reason = joinReason(res.Reason, note)
			}
			return res
		} else {
			res.Reason = joinReason(res.Reason, fmt.Sprintf("作者默认为 %s 但不可用（%s）", d.ID, why))
		}
	}

	// ④ 作者的 fallbacks 链
	for _, c := range cands {
		for _, fb := range c.Fallbacks {
			if me, ok := byID[fb]; ok {
				if why := avail(me.ModelEntry); why == "" {
					res.ModelID, res.Source = me.ID, "fallback"
					res.Reason = joinReason(res.Reason, fmt.Sprintf("按作者声明的 fallbacks 选用 %s", fb))
					return res
				}
			}
		}
	}

	// ⑤ 宿主能力兜底：同能力候选中「最快」的可用者（并告知用户）
	if best, ok := fastest(availableOnly(m, cands)); ok {
		res.ModelID, res.Source = best.ID, "hostFallback"
		res.Reason = joinReason(res.Reason,
			fmt.Sprintf("作者未声明可用默认，宿主兜底选中最快模型 %s（§15.4 须告知用户）", best.ID))
		return res
	}

	res.Source = "none"
	res.Reason = joinReason(res.Reason,
		fmt.Sprintf("%s 能力无可用模型（候选 %d 个全部不可用）", capability, len(cands)))
	return res
}

// availableOnly 过滤出在位性检查通过的候选。
func availableOnly(m *Manager, cands []modelRec) []modelRec {
	var out []modelRec
	for _, c := range cands {
		if m.modelAvailability(c.ModelEntry) == "" {
			out = append(out, c)
		}
	}
	return out
}

// candidateViews 把登记条目转成候选视图（含 unavailable 标注；pinned/effective 由 markEffective 补）。
func (m *Manager) candidateViews(cands []modelRec) []ModelCandidate {
	rm := m.resourceManager()
	out := make([]ModelCandidate, 0, len(cands))
	for _, c := range cands {
		v := ModelCandidate{
			ID: c.ID, Provider: c.Provider, Backend: c.Backend, Quant: c.Quant,
			Path: m.ResolveModelPath(c.Path), Languages: c.Languages, SizeBytes: c.SizeBytes,
			MinVramGB: c.MinVramGB, License: c.License, QualityTier: c.QualityTier,
			ColdStartMs: c.ColdStartMs, Default: c.Default, Fallbacks: c.Fallbacks,
			Companion: c.Companion, DownloadURL: c.DownloadURL,
			State: c.State, Unavailable: m.modelAvailability(c.ModelEntry),
		}
		if rm != nil {
			if e := rm.Get(c.ID); e != nil {
				v.State = e.State
				v.RefCount = e.RefCount
			}
		}
		out = append(out, v)
	}
	return out
}

// CapabilitiesSnapshot 返回全部能力的解析结果（供 §7.4 预加载与 §25 模型选择器）。
func (m *Manager) CapabilitiesSnapshot(pins map[string]string) []ModelResolution {
	caps := m.allCapabilities()
	out := make([]ModelResolution, 0, len(caps))
	for _, c := range caps {
		out = append(out, m.ResolveCapability(c, pins))
	}
	return out
}

// allCapabilities 汇总全部已声明的能力维度（registry.json.models ∪ ResourceMap）。
func (m *Manager) allCapabilities() []string {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	seen := map[string]bool{}
	if rs != nil {
		if f, err := rs.Load(); err == nil {
			for _, me := range f.Models {
				if me.Capability != "" && !me.Companion {
					seen[me.Capability] = true
				}
			}
		}
	}
	if rm := m.resourceManager(); rm != nil {
		for cap := range rm.SnapshotCapabilities() {
			if cap != "" {
				seen[cap] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// markEffective 把解析结果标注回候选视图（供 UI 高亮当前生效项与 pin 项）。
func markEffective(res *ModelResolution) {
	for i := range res.Candidates {
		res.Candidates[i].Pinned = res.Pinned != "" && res.Candidates[i].ID == res.Pinned
		res.Candidates[i].Effective = res.ModelID != "" && res.Candidates[i].ID == res.ModelID
	}
}

func joinReason(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "；" + b
	}
}
