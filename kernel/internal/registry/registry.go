// Package registry 是安装清单与资源状态（registry.json）的唯一写入方。
//
// 对齐《理想架构.md》§8 / §5.1（B 层：宿主独占写）：
//   - 机器生成，禁止手工编辑；写入为原子写（临时文件 + rename）；
//   - 记录插件条目（version / jsonHash / lockHash / cacheDir / depsState / dataDir）
//     与资源条目（kind / locate / launch / resolvedPath / endpoint / verified）；
//   - 删除插件条目时不得删除 dataDir 指向的目录（§5.2 第 4 条）；
//   - 携带 schemaVersion，宿主内置 migrate(v) → v+1 链；未知更高版本拒绝加载。
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CurrentSchemaVersion 本实现支持的 registry.json 版本。
const CurrentSchemaVersion = 1

// Entry 一个插件安装条目（§8 plugins 节）。
type Entry struct {
	Version      string `json:"version,omitempty"`
	Path         string `json:"path,omitempty"` // 相对项目根，如 "plugins/oct.subtitle-tool"
	ManifestHash string `json:"manifestHash,omitempty"`
	JSONHash     string `json:"jsonHash,omitempty"` // §6.3：codeDeps 规范化序列化 hash
	LockHash     string `json:"lockHash,omitempty"`
	CacheDir     string `json:"cacheDir,omitempty"`  // 相对项目根，如 "runtime/python-3.12-xxxx"
	DepsState    string `json:"depsState,omitempty"` // preparing / ready
	DataDir      string `json:"dataDir,omitempty"`   // §5.2：state/plugins/<id>；删除条目不得删此目录
}

// ResourceEntry 一个外部资源条目（§8 resources 节，工具与模型合并登记）。
type ResourceEntry struct {
	Kind         string `json:"kind"`             // tool | model
	Locate       string `json:"locate,omitempty"` // bundled / explicitPath / env
	Launch       string `json:"launch,omitempty"` // jsonConfig / selfManaged / script
	ResolvedPath string `json:"resolvedPath,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Verified     bool   `json:"verified,omitempty"`
}

// File registry.json 顶层结构。
type File struct {
	SchemaVersion int                      `json:"schemaVersion"`
	Plugins       map[string]Entry         `json:"plugins,omitempty"`
	Resources     map[string]ResourceEntry `json:"resources,omitempty"`
}

// Store registry.json 的读写句柄（单实例；写操作原子化并串行化）。
type Store struct {
	path string
	mu   sync.Mutex
}

// New 以 registry.json 路径构造 Store。
func New(path string) *Store { return &Store{path: path} }

// Path 返回 registry.json 的绝对路径（供日志/调试）。
func (s *Store) Path() string { return s.path }

// Load 读取 registry.json；文件缺失返回空清单（nil error），解析后执行迁移链。
// 遇到未知更高版本返回错误（拒绝加载，MUST NOT 静默忽略）。
func (s *Store) Load() (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (*File, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{SchemaVersion: CurrentSchemaVersion}, nil
		}
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("registry parse: %w", err)
	}
	if err := Migrate(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Save 原子写 registry.json（临时文件 + rename），避免崩溃导致清单损坏。
func (s *Store) Save(f *File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == nil {
		f = &File{SchemaVersion: CurrentSchemaVersion}
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = CurrentSchemaVersion
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return s.writeLocked(append(b, '\n'))
}

func (s *Store) writeLocked(b []byte) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Mutate 读-改-写 原子事务：回调在锁内执行，返回 true 才落盘。
func (s *Store) Mutate(fn func(*File) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadLocked()
	if err != nil {
		return err
	}
	if !fn(f) {
		return nil
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = CurrentSchemaVersion
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return s.writeLocked(append(b, '\n'))
}

// Plugin 返回某插件条目；不存在返回 (zero, false)。
func (s *Store) Plugin(id string) (Entry, bool) {
	f, err := s.Load()
	if err != nil {
		return Entry{}, false
	}
	e, ok := f.Plugins[id]
	return e, ok
}

// SetPlugin 更新/新增某插件条目并落盘。
func (s *Store) SetPlugin(id string, e Entry) error {
	return s.Mutate(func(f *File) bool {
		if f.Plugins == nil {
			f.Plugins = map[string]Entry{}
		}
		f.Plugins[id] = e
		return true
	})
}

// RemovePlugin 删除插件条目。按 §5.2 / §8：不触碰 dataDir 指向的目录。
func (s *Store) RemovePlugin(id string) error {
	return s.Mutate(func(f *File) bool {
		if _, ok := f.Plugins[id]; !ok {
			return false
		}
		delete(f.Plugins, id)
		return true
	})
}

// Resource 返回某资源条目；不存在返回 (zero, false)。
func (s *Store) Resource(id string) (ResourceEntry, bool) {
	f, err := s.Load()
	if err != nil {
		return ResourceEntry{}, false
	}
	e, ok := f.Resources[id]
	return e, ok
}

// SetResource 更新/新增某资源条目并落盘。
func (s *Store) SetResource(id string, re ResourceEntry) error {
	return s.Mutate(func(f *File) bool {
		if f.Resources == nil {
			f.Resources = map[string]ResourceEntry{}
		}
		f.Resources[id] = re
		return true
	})
}

// Migrate 对 registry.json 执行 schemaVersion 迁移链。
// 未知更高版本 MUST 拒绝加载（提示升级），MUST NOT 静默忽略未知字段。
func Migrate(f *File) error {
	if f.SchemaVersion == 0 {
		f.SchemaVersion = 1 // 旧文件无版本号：视为 v1
	}
	if f.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("registry schemaVersion %d > supported %d; upgrade kernel", f.SchemaVersion, CurrentSchemaVersion)
	}
	for v := f.SchemaVersion; v < CurrentSchemaVersion; v++ {
		if err := migrateStep(f, v); err != nil {
			return err
		}
	}
	f.SchemaVersion = CurrentSchemaVersion
	return nil
}

// migrateStep v → v+1 单步迁移；当前无历史迁移，预留链位。
func migrateStep(f *File, from int) error {
	switch from {
	default:
		return nil
	}
}
