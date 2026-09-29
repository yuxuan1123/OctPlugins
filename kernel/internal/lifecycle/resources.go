package lifecycle

import (
	"context"
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
			mod := resources.NewModel(dep.ID, dep.Backend, "", "")
			rm.Register(mod)
		case "tool":
			// §14.2：env 默认定位方案（三选一，用户可在设置页改 explicitPath/bundled）。
			rm.Register(resources.NewTool(dep.ID, dep.OptLocate(), dep.DeclaredPath, m.toolsDir()))
		}
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
