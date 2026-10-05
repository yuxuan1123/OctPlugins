package lifecycle

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/octplugin/kernel/internal/registry"
	"github.com/octplugin/kernel/internal/resources"
)

// 本文件：outtool 描述清单（转化域 §5.2 后半 / §5.3）。
//
// §5.2 规定模型来源有两条：tool 在自身 manifest 用 providesModels 声明；
// **outtool 在「outtool 描述清单」里声明**（原文举例：ollama 声明其可用模型）。
// outtool 没有 manifest（§1.4），所以必须有一份独立清单承载它的声明。
//
// 文件位置：<root>/resources/outtools.json（§14 共享资源根，宿主独占写）。
// 加载时机：内核 boot 时随插件登记一起读入（§5.3「宿主加载 tool / outtool 时把声明
// 登记进 registry.json.models，唯一写入方仍是内核 registry 包」）。
//
// 与 tool 声明的区别：provider 是 outtool id，且该 outtool 同时作为 kind=tool 资源
// 登记进 ResourceMap（这样 §14.2 的 bundled/explicitPath/env 三选一定位同样适用）。

// OuttoolDecl 一个 outtool 的声明。
type OuttoolDecl struct {
	ID   string `json:"id,omitempty"` // 缺省取 map key
	Name string `json:"name,omitempty"`
	// Locate §14.2 定位方案：bundled / explicitPath / env（缺省 env）。
	Locate string `json:"locate,omitempty"`
	// DeclaredPath explicitPath 的绝对路径，或 bundled 下的相对名。
	DeclaredPath string `json:"declaredPath,omitempty"`
	// Desc 供 UI 展示。
	Desc string `json:"desc,omitempty"`
	// ProvidesModels 该 outtool 声明提供的模型（复用 tool 的声明结构，§5.2）。
	ProvidesModels []ProvidesModelDecl `json:"providesModels,omitempty"`
}

// OuttoolFile outtools.json 顶层结构。
type OuttoolFile struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Outtools      map[string]OuttoolDecl  `json:"outtools"`
}

// CurrentOuttoolSchemaVersion 本实现支持的 outtools.json 版本。
const CurrentOuttoolSchemaVersion = 1

// DefaultOuttoolPath 返回 outtool 描述清单的默认位置。
func DefaultOuttoolPath(resourcesDir string) string {
	return resourcesDir + string(os.PathSeparator) + "outtools.json"
}

// LoadOuttoolFile 读取 outtool 描述清单。
// 文件缺失返回空清单（Nil error）：这是常态——大多数部署只用 tool 声明模型。
func LoadOuttoolFile(path string) (*OuttoolFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &OuttoolFile{SchemaVersion: CurrentOuttoolSchemaVersion}, nil
		}
		return nil, err
	}
	var f OuttoolFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("outtools.json parse: %w", err)
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = CurrentOuttoolSchemaVersion
	}
	if f.SchemaVersion > CurrentOuttoolSchemaVersion {
		return nil, fmt.Errorf("outtools.json schemaVersion %d > supported %d; upgrade kernel",
			f.SchemaVersion, CurrentOuttoolSchemaVersion)
	}
	return &f, nil
}

// RegisterOuttools 把 outtool 描述清单登记进 ResourceMap 与 registry.json.models（§5.3）。
//
// 幂等：已由 tool 声明的模型 id 不会被 outtool 覆盖（先到先得），避免同一 id 出现
// 两个 provider 造成 §15.4 解析歧义。返回本次新登记的模型数。
func (m *Manager) RegisterOuttools(f *OuttoolFile) int {
	if f == nil || len(f.Outtools) == 0 {
		return 0
	}
	rm := m.resourceManager()
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()

	ids := make([]string, 0, len(f.Outtools))
	for id := range f.Outtools {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 确定性：registry 落盘顺序稳定

	registered := 0
	for _, key := range ids {
		decl := f.Outtools[key]
		id := strings.TrimSpace(decl.ID)
		if id == "" {
			id = strings.TrimSpace(key)
		}
		if id == "" {
			log.Printf("[outtools] 跳过无 id 的条目（key=%q）", key)
			continue
		}
		// §14.2：outtool 也是 kind=tool 资源，按三选一定位。
		if rm != nil && rm.Get(id) == nil {
			locate := strings.TrimSpace(decl.Locate)
			if locate == "" {
				locate = resources.LocateEnv
			}
			rm.Register(resources.NewTool(id, locate, decl.DeclaredPath, m.toolsDir()))
		}
		if rs == nil {
			continue
		}
		for _, pm := range decl.ProvidesModels {
			mid := strings.TrimSpace(pm.ID)
			if mid == "" {
				log.Printf("[outtools] %s 的 providesModels 含空 id，跳过", id)
				continue
			}
			// 先到先得：tool 已声明的同名模型不覆盖（避免 provider 歧义）。
			if _, exists := rs.Model(mid); exists {
				log.Printf("[outtools] 模型 %s 已被声明（provider 先到先得），跳过 outtool %s 的声明", mid, id)
				continue
			}
			if err := rs.SetModel(mid, registry.ModelEntry{
				Provider:       id,
				Capability:     pm.Capability,
				Backend:        firstNonEmpty(pm.Backend, id),
				Path:           pm.Path,
				Quant:          pm.Quant,
				Languages:      pm.Languages,
				SizeBytes:      pm.SizeBytes,
				MinVramGB:      pm.MinVramGB,
				License:        pm.License,
				QualityTier:    pm.QualityTier,
				ColdStartMs:    pm.ColdStartMs,
				Default:        pm.Default,
				Fallbacks:      pm.Fallbacks,
				Companion:      pm.Companion,
				DownloadURL:    pm.DownloadURL,
				DownloadSHA256: pm.DownloadSHA256,
				State:          "registered",
			}); err != nil {
				log.Printf("[outtools] 登记模型 %s 失败: %v", mid, err)
				continue
			}
			// 模型条目也进 ResourceMap：backend 指向该 outtool，宿主可 Acquire。
			// 注意 outtool 不是常驻进程，其模型加载由 outtool 自身管理，
			// 故不注入 model.ensure 钩子（默认「登记即就绪」，Acquire 只确保 outtool 可定位）。
			if rm != nil && rm.Get(mid) == nil {
				mod := resources.NewModel(mid, id, pm.Capability, pm.Quant, pm.Path)
				mod.Companion = pm.Companion
				rm.Register(mod)
			}
			registered++
		}
	}
	if registered > 0 {
		log.Printf("[outtools] 已登记 %d 个 outtool 提供的模型（provider=outtool）", registered)
	}
	return registered
}

// RegisterOuttoolsFrom 读取 <resourcesDir>/outtools.json 并登记（缺文件即静默跳过）。
// 解析失败只记日志不阻断 boot：outtool 声明是可选增强，不该让内核起不来。
func (m *Manager) RegisterOuttoolsFrom(resourcesDir string) int {
	path := DefaultOuttoolPath(resourcesDir)
	f, err := LoadOuttoolFile(path)
	if err != nil {
		log.Printf("[outtools] 读取 %s 失败（已跳过）: %v", path, err)
		return 0
	}
	return m.RegisterOuttools(f)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
