package lifecycle

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/octplugin/kernel/internal/registry"
	"github.com/octplugin/kernel/internal/resources"
)

// 本文件：装配《理想架构.md》§14 外部共享资源（ResourceMap）到插件生命周期。
//
//   - 插件登记（register）时，externalDependencies 声明被解析并注册进 ResourceMap；
//   - 插件启动（spawn）前，按 §10.2 ② EnsureResources 对 profile.requiresResources
//     与 externalDependencies 执行 Acquire（工具派生 / 模型就绪，引用计数 +1）；
//   - 插件停止（recycle / StopPlugin / StopAll / 崩溃退出）后 Release（§14.5）；
//   - 资源定位/状态登记落盘 registry.json 的 resources 节（§8，唯一写入方 SetResource）。

// SetResourceManager 注入 §14 ResourceMap（内核装配时调用）。
func (m *Manager) SetResourceManager(rm *resources.Manager) {
	m.mu.Lock()
	m.resources = rm
	m.mu.Unlock()
}

// SetResourcesDir 注入 tools/ 根（§14.2 bundled 定位用）。
func (m *Manager) SetResourcesDir(dir string) {
	m.mu.Lock()
	m.toolsRoot = dir
	m.mu.Unlock()
}

// registerResourceDeps 把插件的 externalDependencies 声明注册进 ResourceMap。
// 幂等：同 id 首次注册，后续复用已有条目（§14.5 启动前查表复用）。
func (m *Manager) registerResourceDeps(mf Manifest) {
	rm := m.resourceManager()
	if rm == nil {
		return
	}
	for _, dep := range mf.ExternalDeps {
		// 已在表中 → 直接复用，不重复登记。
		if rm.Get(dep.ID) != nil {
			continue
		}
		switch strings.ToLower(dep.Kind) {
		case "model":
			// externalDependencies 里直接声明的模型（无 providesModels 元信息）：
			// capability 为空 → 按普通资源 id 引用计数，不参与 §10.1 能力维度上限。
			mod := resources.NewModel(dep.ID, dep.Backend, "", "", "")
			rm.Register(mod)
		case "tool":
			// §14.2：env 默认定位方案（三选一，用户可在设置页改 explicitPath/bundled）。
			rm.Register(resources.NewTool(dep.ID, dep.OptLocate(), dep.DeclaredPath, m.toolsDir()))
		}
	}
}

// registerProvidedModels 把 tool 的 providesModels 声明登记进 registry.json 的 models 节
// （转化域 §三：模型由 tool / outtool 声明提供，非目录扫描发现；宿主加载，tool 不自行加载），
// 并把模型条目注册进 ResourceMap（backend 指向提供它的 tool id，宿主可经 registry.models.acquire
// 触发就绪登记；模型就绪 = 登记就绪，MVP 不真实加载权重）。
// 幂等：同 id 已登记则跳过（保留既有 state）。
func (m *Manager) registerProvidedModels(mf Manifest) {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil || len(mf.ProvidesModels) == 0 {
		return
	}
	// backend 指向提供者插件 id，故先把该插件自身登记成 ResourceMap 的 tool 条目，
	// 否则模型 Acquire 时 Acquire(backend) 会查表落空（resource "…" not registered）。
	m.registerProviderTool(mf.ID)
	for _, pm := range mf.ProvidesModels {
		id := pm.ID
		if id == "" {
			continue
		}
		rm := m.resourceManager()
		// ResourceMap 已注册则跳过（幂等）；registry.json 已有条目保留既有 state，不覆盖。
		if rm != nil && rm.Get(id) != nil {
			continue
		}
		if _, ok := rs.Model(id); !ok {
			_ = rs.SetModel(id, registry.ModelEntry{
				Provider:    mf.ID,
				Capability:  pm.Capability,
				Backend:     pm.Backend,
				Path:        pm.Path,
				Quant:       pm.Quant,
				Languages:   pm.Languages,
				SizeBytes:   pm.SizeBytes,
				MinVramGB:   pm.MinVramGB,
				License:     pm.License,
				QualityTier: pm.QualityTier,
				ColdStartMs: pm.ColdStartMs,
				Default:     pm.Default,
				Fallbacks:   pm.Fallbacks,
				Companion:   pm.Companion,
				DownloadURL: pm.DownloadURL,
				DownloadSHA256: pm.DownloadSHA256,
				State:       "registered",
			})
		}
		// ResourceMap 登记：backend 指向提供它的 tool id（Acquire 模型时先确保 tool 进程），
		// capability 用于 §10.1 的能力维度上限；伴随模型不占该上限。
		if rm != nil {
			mod := resources.NewModel(id, mf.ID, pm.Capability, pm.Quant, pm.Path)
			mod.Companion = pm.Companion
			mod.SetModelHooks(m.modelHooksFor(id, mf.ID))
			rm.Register(mod)
		}
	}
}

// registerProviderTool 把「提供模型的插件」自身登记成 tool 资源（模型 backend 的落点）。
//
// 模型条目的 backend 是提供它的插件 id（见 registerProvidedModels 的 mf.ID），资源层要求
// Acquire(backend) 能查表命中，否则每次模型 Acquire 都会打一条
// `backend … acquire degraded: resource "…" not registered`。这里补一条 tool 条目：
//   - start：GetOrStart(插件) —— 与 registry.call 的懒启动同一条路径，模型就绪前先保证 provider 在线；
//   - stop：空操作 —— 插件生命周期归 lifecycle（recycle / StopPlugin / 退出清理），资源层不擅自停进程。
//
// 已存在同 id 条目（例如某插件用 externalDependencies 声明过它）则复用，不覆盖其定位方案。
func (m *Manager) registerProviderTool(providerID string) {
	rm := m.resourceManager()
	if rm == nil || providerID == "" {
		return
	}
	if rm.Get(providerID) != nil {
		return
	}
	tool := resources.NewPluginTool(providerID, func(context.Context) error {
		pl, err := m.GetOrStart(providerID)
		if err != nil {
			return fmt.Errorf("provider plugin %s: %w", providerID, err)
		}
		if !pl.Alive() {
			return fmt.Errorf("provider plugin %s is down", providerID)
		}
		return nil
	}, nil)
	rm.Register(tool)
	m.mu.Lock()
	if m.autoTools == nil {
		m.autoTools = map[string]struct{}{}
	}
	m.autoTools[providerID] = struct{}{}
	m.mu.Unlock()
}

// unregisterProvidedModels 卸载 tool 时收回其模型声明（registry.json.models + ResourceMap）。
func (m *Manager) unregisterProvidedModels(id string) {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	rm := m.resourceManager()
	if rm != nil {
		// 摘除该 tool 提供的全部模型条目（引用归零由资源层处理；先收集再改 map）。
		var doomed []string
		rm.Range(func(mid, kind, _ string, _ int) {
			if kind == "model" {
				if e := rm.Get(mid); e != nil && e.Backend == id {
					doomed = append(doomed, mid)
				}
			}
		})
		for _, mid := range doomed {
			rm.Unregister(mid)
		}
	}
	// 收回本层自动登记的 provider tool 条目（外部声明的同 id tool 不在此列）。
	if rm != nil {
		m.mu.Lock()
		_, auto := m.autoTools[id]
		delete(m.autoTools, id)
		m.mu.Unlock()
		if auto {
			rm.Unregister(id)
		}
	}
	if rs == nil {
		return
	}
	if err := rs.RemoveModelsByProvider(id); err != nil {
		log.Printf("[kernel] registry: remove models of %s: %v", id, err)
	}
}

// acquireProfileResources 按 §10.2 ② 对 profile 的 requiresResources 执行 Acquire。
// 未配置 ResourceMap 时跳过（等价旧行为，不阻塞启动）。
func (m *Manager) acquireProfileResources(mf Manifest) error {
	rm := m.resourceManager()
	if rm == nil {
		return nil
	}
	// externalDependencies 总声明：作为该插件依赖的资源集合。
	for _, dep := range mf.ExternalDeps {
		if err := rm.Acquire(context.Background(), dep.ID); err != nil {
			// optional=true：定位/启动失败仅降级（日志告警），不阻塞插件启动；
			// 工具内部运行时按需自行探测并给出 E_TOOL_UNAVAILABLE。
			if dep.Optional {
				log.Printf("[kernel] resource %s (optional) unavailable: %v", dep.ID, err)
				continue
			}
			return err
		}
	}
	// profile.requiresResources 里的资源若未在 externalDependencies 声明，也一并获取。
	for _, id := range profileRequires(mf) {
		if rm.Get(id) == nil {
			continue // 未声明不注册的资源不可获取（避免隐式派生无声明工具）
		}
		if err := rm.Acquire(context.Background(), id); err != nil {
			return err
		}
	}
	return nil
}

// releaseResourcesFor 按插件的资源声明 Release（§14.5：引用计数 -1；
// 计数归零按策略处置：onDemand 立即停/卸载，resident 保持）。
func (m *Manager) releaseResourcesFor(mf Manifest) {
	rm := m.resourceManager()
	if rm == nil {
		return
	}
	for _, dep := range mf.ExternalDeps {
		rm.Release(dep.ID)
	}
	for _, id := range profileRequires(mf) {
		if rm.Get(id) != nil {
			rm.Release(id)
		}
	}
}

// syncResourceRegistry 把已登记资源的定位/状态记录落盘 registry.json（§8 resources 节）。
func (m *Manager) syncResourceRegistry() {
	rm := m.resourceManager()
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rm == nil || rs == nil {
		return
	}
	rm.Range(func(id string, kind string, state string, refCount int) {
		_ = rs.SetResource(id, registry.ResourceEntry{
			Kind: kind,
			// locate/launch 由用户/registry 设置，此处不覆写用户显式配置：
			// 仅登记内核侧确证的定位来源为空（env 默认），resolvedPath 由工具侧维护。
			Verified: refCount > 0,
		})
	})
}

// SyncResourceRegistry 供装配层在 Registration 完成后触发 registry.json 资源节落盘（§8）。
func (m *Manager) SyncResourceRegistry() {
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil {
		return
	}
	// 登记后调用，确保 externalDependencies 已入 ResourceMap。
	m.syncResourceRegistry()
}

func profileRequires(mf Manifest) []string {
	name := mf.Defaults.Profile
	if name == "" {
		for k := range mf.Profiles {
			name = k
			break
		}
	}
	if p, ok := mf.Profiles[name]; ok {
		return p.RequiresResources
	}
	return nil
}

func (m *Manager) resourceManager() *resources.Manager {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resources
}

func (m *Manager) toolsDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.toolsRoot
}
