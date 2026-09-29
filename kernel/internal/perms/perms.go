// 权限位图引擎（FR-7）。
// 模型：Grant = 插件已声明且用户已授权的某项权限。
// Marshalable 位：file_read, file_write, network, execute_command,
//
//	local_model, spawn_process。
package perms

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// 权限位定义（FR-7 至少要求这六项）。
const (
	FileRead       = "file_read"
	FileWrite      = "file_write"
	Network        = "network"
	ExecuteCommand = "execute_command"
	LocalModel     = "local_model"
	SpawnProcess   = "spawn_process"
)

var All = []string{FileRead, FileWrite, Network, ExecuteCommand, LocalModel, SpawnProcess}

var ErrNotGranted = errors.New("permission not granted")

// Gate 权限清单：pluginId -> 已授权权限集合。
type Gate struct {
	mu sync.Mutex
	// grants: pluginId -> {perm: true}
	grants map[string]map[string]bool
	// declared: pluginId -> {perm: true}（manifest 声明值，权限最小化的基准）
	declared map[string]map[string]bool
	// 高危险度操作，需二次确认（即使已授权也走确认，FR-10）
	highRisk map[string]bool
	path     string // 授权持久化文件 state/perms.json
}

func NewGate(path string) *Gate {
	g := &Gate{
		grants:   map[string]map[string]bool{},
		declared: map[string]map[string]bool{},
		highRisk: map[string]bool{
			ExecuteCommand: true,
			SpawnProcess:   true,
			FileWrite:      true,
		},
		path: path,
	}
	g.load()
	return g
}

type record struct {
	Declared map[string]bool `json:"declared"`
	Granted  map[string]bool `json:"granted"`
}

func (g *Gate) load() {
	b, err := os.ReadFile(g.path)
	if err != nil {
		return
	}
	var m map[string]record
	if err := json.Unmarshal(b, &m); err != nil {
		return
	}
	for id, r := range m {
		g.declared[id] = r.Declared
		g.grants[id] = r.Granted
	}
}

func (g *Gate) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(g.path), 0o755); err != nil {
		return err
	}
	m := map[string]record{}
	for id := range g.grants {
		m[id] = record{Declared: g.declared[id], Granted: g.grants[id]}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(g.path, b, 0o600)
}

// Declare 登记插件声明权限（仅可授权声明范围内的权限，FR-7 最小权限）。
func (g *Gate) Declare(pluginID string, perms []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := map[string]bool{}
	for _, p := range perms {
		d[p] = true
	}
	g.declared[pluginID] = d
	if g.grants[pluginID] != nil {
		// 声明的权限被移除 → 收回；去掉未声明的授权
		for p := range g.grants[pluginID] {
			if !d[p] {
				delete(g.grants[pluginID], p)
			}
		}
	}
	_ = g.saveLocked()
}

// Authorize 一次性授予 ≥0 项权限（仅限已声明项）。plugins 为空可配置热键等。
func (g *Gate) Authorize(pluginID string, perms []string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runtimeAuthorize(pluginID, perms)
	return g.saveLocked()
}

func (g *Gate) runtimeAuthorize(pluginID string, perms []string) {
	if _, ok := g.grants[pluginID]; !ok {
		g.grants[pluginID] = map[string]bool{}
	}
	for _, p := range perms {
		if !g.declared[pluginID][p] {
			continue // 最小权限：只许授声明内的
		}
		g.grants[pluginID][p] = true
	}
}

// Revoke 动态收回某项权限（即时生效，FR-7）。
func (g *Gate) Revoke(pluginID, perm string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.grants[pluginID] == nil {
		return nil
	}
	delete(g.grants[pluginID], perm)
	return g.saveLocked()
}

// Check 每次敏感操作前调用；未授权返回 ErrNotGranted。
func (g *Gate) Check(pluginID, perm string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.grants[pluginID] == nil || !g.grants[pluginID][perm] {
		return ErrNotGranted
	}
	return nil
}

// IsHighRisk 判断是否需要二次确认。
func (g *Gate) IsHighRisk(perm string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.highRisk[perm]
}

// Declared / Granted 供宿主展示授权状态。
func (g *Gate) Subset(pluginID string) (declared, granted []string, highRisk []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for p := range g.declared[pluginID] {
		declared = append(declared, p)
	}
	for p := range g.grants[pluginID] {
		granted = append(granted, p)
	}
	for p := range g.highRisk {
		highRisk = append(highRisk, p)
	}
	return
}
