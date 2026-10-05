package lifecycle

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/octplugin/kernel/internal/registry"
)

// 本文件：面向宿主/渲染进程的模型管理 RPC 支撑（转化域 §三 / §四）。
//
//   - 模型由 tool / outtool 声明提供（providesModels），内核登记进 registry.json.models
//     并注册进 ResourceMap（backend = 提供它的 tool id）；
//   - 加载权在宿主：宿主经 registry.models.acquire / release 触发 ResourceMap 的就绪登记与释放；
//   - MVP 语义（§14.11）：模型就绪 = 登记就绪，宿主不真实加载权重、不做显存调度。

// ModelInfo 一份模型的登记 + 资源状态（registry.json.models 合并 ResourceMap 运行时状态）。
type ModelInfo struct {
	ID          string   `json:"id"`
	Provider    string   `json:"provider"`
	Capability  string   `json:"capability,omitempty"`
	Backend     string   `json:"backend"`
	Path        string   `json:"path,omitempty"`
	Quant       string   `json:"quant,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	SizeBytes   int64    `json:"sizeBytes,omitempty"`
	MinVramGB   float64  `json:"minVramGB,omitempty"`
	License     string   `json:"license,omitempty"`
	QualityTier string   `json:"qualityTier,omitempty"`
	ColdStartMs int      `json:"coldStartMs,omitempty"`
	Default     bool     `json:"default,omitempty"`     // 作者声明的该能力默认（§15.4）
	Fallbacks   []string `json:"fallbacks,omitempty"`   // 作者声明的降级序列（§15.4）
	Companion   bool     `json:"companion,omitempty"`   // 伴随模型（不占能力维度上限）
	// DownloadURL 非空表示宿主可代为下载（§26）；UI 据此给出「下载」入口并请求用户同意。
	DownloadURL string `json:"downloadUrl,omitempty"`
	State       string `json:"state"`                 // registered / loading / ready / failed（运行态覆盖登记态）
	RefCount    int    `json:"refCount"`
	Pinned      bool   `json:"pinned,omitempty"`      // 被用户 pin 为该能力的默认
	Effective   bool   `json:"effective,omitempty"`   // 当前该能力的生效模型
	Unavailable string `json:"unavailable,omitempty"` // 非空 = 不可用原因（§25 灰显说明）
}

// ModelsSnapshot 返回全部模型快照（按 capability 分组排序：capability, id）。
// pins 为用户能力级 pin（capability → modelId）；未 pin 传 nil 亦可。
func (m *Manager) ModelsSnapshot(pins map[string]string) []ModelInfo {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	rm := m.resourceManager()
	out := []ModelInfo{}
	if rs == nil {
		return out
	}
	f, err := rs.Load()
	if err != nil {
		log.Printf("[kernel] models snapshot: %v", err)
		return out
	}
	// 先算每个能力的生效模型，用于标注 effective。
	effective := map[string]string{}
	for _, cap := range m.allCapabilities() {
		if r := m.ResolveCapability(cap, pins); r.ModelID != "" {
			effective[cap] = r.ModelID
		}
	}
	for id, me := range f.Models {
		info := ModelInfo{
			ID: id, Provider: me.Provider, Capability: me.Capability,
			Backend: me.Backend, Path: m.ResolveModelPath(me.Path), Quant: me.Quant,
			Languages: me.Languages, SizeBytes: me.SizeBytes, MinVramGB: me.MinVramGB,
			License: me.License, QualityTier: me.QualityTier, ColdStartMs: me.ColdStartMs,
			Default: me.Default, Fallbacks: me.Fallbacks, Companion: me.Companion,
			DownloadURL: me.DownloadURL,
			State:       me.State,
		}
		info.Pinned = pins != nil && pins[me.Capability] == id
		info.Effective = me.Capability != "" && effective[me.Capability] == id
		if me.Capability != "" {
			info.Unavailable = m.modelAvailability(me)
		}
		if rm != nil {
			if e := rm.Get(id); e != nil {
				info.State = e.State
				info.RefCount = e.RefCount
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Capability != out[j].Capability {
			return out[i].Capability < out[j].Capability
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// AcquireModel 由宿主触发模型加载（转化域 §四：加载权在宿主）。
//
// 语义已升级为「确保就绪」：已就绪直接返回；否则若该能力已有快模型在服务，
// 按 §12 走 handover（立即返回，快模型继续顶上），否则同步加载。
func (m *Manager) AcquireModel(id string) error {
	rm := m.resourceManager()
	if rm == nil {
		return fmt.Errorf("resource map unavailable")
	}
	if rm.Get(id) == nil {
		return fmt.Errorf("model %q not registered", id)
	}
	_, err := m.EnsureModelDirect(context.Background(), id)
	return err
}

// ReleaseModel 按 id 释放模型（引用 -1；归零按策略处置）。
func (m *Manager) ReleaseModel(id string) {
	rm := m.resourceManager()
	if rm == nil {
		return
	}
	rm.Release(id)
}

// modelByID 取单条模型登记（供 RPC 返回）。
func (m *Manager) modelByID(id string) (registry.ModelEntry, bool) {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil {
		return registry.ModelEntry{}, false
	}
	return rs.Model(id)
}
