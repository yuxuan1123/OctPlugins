package lifecycle

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/octplugin/kernel/internal/perms"
)

// 本文件：转化域 §23 的 manifest 契约校验。
//
// §23.1 requiresCapabilities：conversion 只声明「需要哪些能力维度」，不写具体 model id。
//   → 内核据此约束 gate.model_ensure：插件只能请求自己声明过的能力。
//
// §23.2 spawn：可派生的 outtool id 白名单；net 默认 false。
//   → 白名单条目 MUST 与 externalDependencies 对得上（否则是笔误或越权声明）；
//     声明联网 MUST 同时声明 network 权限，否则契约自相矛盾。
//
// §23.3 m3u8 远程拉取做独立可选 profile：由 ProfileDecl{Optional:true, Net:true} 表达。

// effectiveNet 返回某插件在给定 profile 下是否允许联网（§23.2/§23.3）。
// 优先级：profile.Net（显式）> manifest.Net（缺省 false）。
func effectiveNet(mf Manifest, profile string) bool {
	if p, ok := mf.Profiles[profile]; ok && p.Net != nil {
		return *p.Net
	}
	return mf.Net
}

// validateManifestContract 登记期校验 §23 契约。返回错误即拒绝登记该插件。
//
// 设计取舍：只拒绝「自相矛盾」的声明（白名单里有未声明的 outtool、要联网却不声明权限），
// 不对能力维度是否真有 provider 做硬失败——那取决于用户装了哪些 tool，
// 属于运行期状态（由 §15.4 解析给出 source=none 并告知用户），不是 manifest 缺陷。
func validateManifestContract(mf Manifest) error {
	// §23.2：spawn 白名单必须能在 externalDependencies 里找到对应 outtool。
	if len(mf.Spawn) > 0 {
		declared := map[string]bool{}
		for _, dep := range mf.ExternalDeps {
			if strings.EqualFold(strings.TrimSpace(dep.Kind), "tool") {
				declared[strings.TrimSpace(dep.ID)] = true
			}
		}
		for _, id := range mf.Spawn {
			spawnID := strings.TrimSpace(id)
			if spawnID == "" {
				return fmt.Errorf("spawn 白名单含空 id（插件 %s）", mf.ID)
			}
			if !declared[spawnID] {
				return fmt.Errorf(
					"spawn 白名单里的 outtool %q 未在 externalDependencies 中声明（插件 %s）；"+
						"§23.2 要求白名单条目 MUST 是已声明的 outtool id", spawnID, mf.ID)
			}
		}
	}

	// §23.2：manifest 级 net=true 必须声明 network 权限。
	if mf.Net && !hasPerm(mf.Permissions, perms.Network) {
		return fmt.Errorf("manifest 声明 net=true 但未声明 %q 权限（插件 %s）；§23.2 要求二者一致",
			perms.Network, mf.ID)
	}
	// §23.3：profile 级 net=true 同样必须声明 network 权限。
	for name, p := range mf.Profiles {
		if p.Net != nil && *p.Net && !hasPerm(mf.Permissions, perms.Network) {
			return fmt.Errorf("profile %q 声明 net=true 但 manifest 未声明 %q 权限（插件 %s）",
				name, perms.Network, mf.ID)
		}
	}

	// §23.1：requiresCapabilities 至少不应重复声明同一能力（重复通常是复制粘贴笔误）。
	seen := map[string]bool{}
	for _, c := range mf.RequiresCapabilities {
		capability := strings.TrimSpace(c)
		if capability == "" {
			return fmt.Errorf("requiresCapabilities 含空值（插件 %s）", mf.ID)
		}
		if seen[capability] {
			log.Printf("[%s] requiresCapabilities 重复声明 %q（已去重处理）", mf.ID, capability)
		}
		seen[capability] = true
	}
	return nil
}

func hasPerm(list []string, want string) bool {
	for _, p := range list {
		if strings.EqualFold(strings.TrimSpace(p), want) {
			return true
		}
	}
	return false
}

// requiresCapability 判断插件是否声明了某能力（§23.1）。
// 未声明 requiresCapabilities 的插件不做能力白名单约束（向后兼容，等价旧行为）。
func requiresCapability(mf Manifest, capability string) (declared bool, ok bool) {
	if len(mf.RequiresCapabilities) == 0 {
		return false, true
	}
	for _, c := range mf.RequiresCapabilities {
		if strings.EqualFold(strings.TrimSpace(c), capability) {
			return true, true
		}
	}
	return true, false
}

// CapabilityContract 供宿主/UI 查看某插件的 §23 契约（诊断与设置页展示）。
type CapabilityContract struct {
	PluginID             string   `json:"pluginId"`
	RequiresCapabilities []string `json:"requiresCapabilities,omitempty"`
	Spawn                []string `json:"spawn,omitempty"`
	Net                  bool     `json:"net"`
	NetworkAllowed       bool     `json:"networkAllowed"` // 是否声明了 network 权限
	OptionalProfiles     []string `json:"optionalProfiles,omitempty"`
	Profiles             []string `json:"profiles,omitempty"`
}

// Contracts 返回全部已登记插件的 §23 契约（供宿主 UI 展示「这个插件要什么」）。
func (m *Manager) Contracts() []CapabilityContract {
	m.mu.Lock()
	regs := make([]*reg, 0, len(m.regs))
	for _, r := range m.regs {
		regs = append(regs, r)
	}
	m.mu.Unlock()

	out := make([]CapabilityContract, 0, len(regs))
	for _, r := range regs {
		if r == nil {
			continue
		}
		mf := r.mf
		if len(mf.RequiresCapabilities) == 0 && len(mf.Spawn) == 0 && len(mf.Profiles) == 0 {
			continue // 只列出有契约可言的插件，避免刷屏
		}
		c := CapabilityContract{
			PluginID:             mf.ID,
			RequiresCapabilities: mf.RequiresCapabilities,
			Spawn:                mf.Spawn,
			Net:                  mf.Net,
			NetworkAllowed:       hasPerm(mf.Permissions, perms.Network),
		}
		for name, p := range mf.Profiles {
			c.Profiles = append(c.Profiles, name)
			if p.Optional {
				c.OptionalProfiles = append(c.OptionalProfiles, name)
			}
		}
		sort.Strings(c.Profiles)
		sort.Strings(c.OptionalProfiles)
		out = append(out, c)
	}
	return out
}
