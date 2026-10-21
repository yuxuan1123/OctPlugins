// Package config 承载 user-settings.json 的读写与插件设置冲突消解（《理想架构.md》§5.3/§9）。
//
// 文件定位：config/user-settings.json（C 层，唯一人工可写）。
// 冲突消解：字段缺失时逐级回落 user-settings → manifest.defaults → 内置默认，
// 合并产出只读内存态 EffectiveConfig。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/octplugin/kernel/internal/lifecycle"
)

// PluginCfg 单个插件的用户配置（settings + 选中的 profile）。
type PluginCfg struct {
	Settings map[string]any `json:"settings,omitempty"`
	Profile  string         `json:"profile,omitempty"`
}

// CapabilityCfg 单个能力维度的用户偏好（转化域 §8.1：设置级 pin）。
type CapabilityCfg struct {
	Pin string `json:"pin,omitempty"`
}

// File user-settings.json 的内存结构。
type File struct {
	Plugins map[string]PluginCfg `json:"plugins,omitempty"`
	UI      map[string]any       `json:"ui,omitempty"`
	// Capabilities 能力维度偏好：capabilities.<name>.pin（§8.1）。
	Capabilities map[string]CapabilityCfg `json:"capabilities,omitempty"`
	// ModelsRootDir 模型存储根（转化域 §20.5）；空 = 未配置，回落 lifecycle.DefaultModelsRoot。
	// 字段名不与 ModelsRoot() 方法同名（Go 不允许字段与方法同名）。
	ModelsRootDir string `json:"modelsRoot,omitempty"`
}

// Pins 返回「能力名 → 用户 pin 的 modelId」（§8.1）。空值一律不返回。
func (f *File) Pins() map[string]string {
	out := map[string]string{}
	if f == nil {
		return out
	}
	for cap, c := range f.Capabilities {
		if v := strings.TrimSpace(c.Pin); v != "" {
			out[cap] = v
		}
	}
	return out
}

// ModelsRoot 返回用户配置的模型存储根（§20.5）；未配置返回空串，由调用方回落默认。
func (f *File) ModelsRoot() string {
	if f == nil {
		return ""
	}
	return strings.TrimSpace(f.ModelsRootDir)
}

// SchemaProps 提取 JSON Schema 的 properties 字段名（§17.2 A：settingsSchema 白名单）。
// 非法/空 schema 返回 nil；结果按字典序稳定输出（便于测试与回显）。
func SchemaProps(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	out := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Effective 冲突消解后的生效配置（只读）。
type Effective struct {
	Profile  string
	Settings map[string]any
}

// Settings user-settings.json 的读写句柄（线程安全）。
type Settings struct {
	mu   sync.Mutex
	path string
}

// NewSettings 创建 settings 句柄，path 为 user-settings.json 绝对路径。
func NewSettings(path string) *Settings {
	return &Settings{path: path}
}

// Load 读取 user-settings.json；文件不存在返回空 File（不报错）。
func (s *Settings) Load() (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Save 原子写入 user-settings.json（临时文件 + rename）。
func (s *Settings) Save(f *File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(f)
}

// SetPin 写/清某能力维度的模型 pin（转化域 §8.1）。
// modelID 为空 = 清除该能力的 pin；整个读-改-写在锁内完成（sync.Mutex 不可重入，
// 故经 loadLocked/saveLocked，不调用会再次加锁的 Load/Save）。
func (s *Settings) SetPin(capability, modelID string) error {
	capability = strings.TrimSpace(capability)
	if capability == "" {
		return fmt.Errorf("capability required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadLocked()
	if err != nil {
		return err
	}
	if f.Capabilities == nil {
		f.Capabilities = map[string]CapabilityCfg{}
	}
	if v := strings.TrimSpace(modelID); v == "" {
		delete(f.Capabilities, capability)
	} else {
		f.Capabilities[capability] = CapabilityCfg{Pin: v}
	}
	return s.saveLocked(f)
}

// SetModelsRoot 写/清模型存储根（转化域 §20.5）。空串 = 清除，回落宿主默认。
func (s *Settings) SetModelsRoot(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.loadLocked()
	if err != nil {
		return err
	}
	f.ModelsRootDir = strings.TrimSpace(dir)
	return s.saveLocked(f)
}

func (s *Settings) loadLocked() (*File, error) {
	f := &File{Plugins: map[string]PluginCfg{}, UI: map[string]any{}}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, err
	}
	if f.Plugins == nil {
		f.Plugins = map[string]PluginCfg{}
	}
	if f.UI == nil {
		f.UI = map[string]any{}
	}
	return f, nil
}

// saveLocked 原子写入（调用方须已持有 s.mu）。
func (s *Settings) saveLocked(f *File) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ResolvePlugin 冲突消解：user settings 优先，其次 manifest.defaults，最后空 map。
// profile 同理：user.Profile → manifest.Defaults.Profile → ""。
func ResolvePlugin(pluginID string, mf lifecycle.Manifest, user PluginCfg) Effective {
	profile := user.Profile
	if profile == "" {
		profile = mf.Defaults.Profile
	}

	settings := map[string]any{}
	// 先放作者 defaults（mf.Defaults 里没有 settings 字段，留空）
	// 再用 user 覆盖
	for k, v := range user.Settings {
		settings[k] = v
	}
	return Effective{Profile: profile, Settings: settings}
}