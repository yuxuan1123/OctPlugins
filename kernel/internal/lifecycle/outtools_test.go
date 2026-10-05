package lifecycle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/octplugin/kernel/internal/perms"
	"github.com/octplugin/kernel/internal/registry"
	"github.com/octplugin/kernel/internal/resources"
)

// 本文件：转化域 §5.2/§5.3（outtool 描述清单声明模型）的行为锁定。

func outEnv(t *testing.T) (*Manager, *registry.Store, string) {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(dir, perms.NewGate(filepath.Join(dir, "perms.json")))
	rs := registry.New(filepath.Join(dir, "registry.json"))
	m.SetRegistryStore(rs)
	rm := resources.NewManager()
	m.SetResourceManager(rm)
	return m, rs, dir
}

// 文件缺失 → 空清单且不报错（常态）。
func TestOuttoolFileMissingIsEmpty(t *testing.T) {
	f, err := LoadOuttoolFile(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(f.Outtools) != 0 {
		t.Fatalf("expected empty outtools, got %d", len(f.Outtools))
	}
}

// 更高 schemaVersion → 拒绝加载（与 registry/user-settings 同规）。
func TestOuttoolFileFutureVersionRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outtools.json")
	if err := os.WriteFile(p, []byte(`{"schemaVersion":99,"outtools":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOuttoolFile(p); err == nil {
		t.Fatal("future schemaVersion must be rejected")
	}
}

// §5.2/§5.3：outtool 声明的模型登记进 registry.json.models，provider = outtool id，
// 且该 outtool 自身作为 kind=tool 资源进 ResourceMap（可按 §14.2 定位）。
func TestOuttoolModelsRegistered(t *testing.T) {
	m, rs, _ := outEnv(t)
	f := &OuttoolFile{SchemaVersion: 1, Outtools: map[string]OuttoolDecl{
		"ollama": {
			Name: "Ollama", Locate: "env", Desc: "本地模型服务",
			ProvidesModels: []ProvidesModelDecl{
				{ID: "qwen2.5:3b", Capability: "translate", Languages: []string{"zh", "en"},
					SizeBytes: 2000000000, QualityTier: "small", ColdStartMs: 4000},
			},
		},
	}}
	n := m.RegisterOuttools(f)
	if n != 1 {
		t.Fatalf("registered = %d, want 1", n)
	}
	me, ok := rs.Model("qwen2.5:3b")
	if !ok {
		t.Fatal("model should be registered")
	}
	if me.Provider != "ollama" {
		t.Fatalf("provider = %q, want ollama（§5.2 provider 为提供它的 outtool id）", me.Provider)
	}
	if me.Capability != "translate" || me.ColdStartMs != 4000 {
		t.Fatalf("declaration not carried over: %+v", me)
	}
	// backend 缺省回落 provider
	if me.Backend != "ollama" {
		t.Fatalf("backend = %q, want ollama（缺省取 provider）", me.Backend)
	}
	// outtool 自身进 ResourceMap 且为 kind=tool
	rm := m.resourceManager()
	e := rm.Get("ollama")
	if e == nil {
		t.Fatal("outtool should be registered in ResourceMap")
	}
	if e.Kind != resources.KindTool {
		t.Fatalf("outtool kind = %v, want KindTool", e.Kind)
	}
}

// 先到先得：tool 已声明的同名模型不被 outtool 覆盖（避免 §15.4 解析出现 provider 歧义）。
func TestOuttoolDoesNotOverrideToolDeclaredModel(t *testing.T) {
	m, rs, _ := outEnv(t)
	if err := rs.SetModel("hy_mt", registry.ModelEntry{
		Provider: "hy-mt-server", Capability: "translate", State: "registered",
	}); err != nil {
		t.Fatal(err)
	}
	f := &OuttoolFile{SchemaVersion: 1, Outtools: map[string]OuttoolDecl{
		"ollama": {ProvidesModels: []ProvidesModelDecl{{ID: "hy_mt", Capability: "translate"}}},
	}}
	if n := m.RegisterOuttools(f); n != 0 {
		t.Fatalf("registered = %d, want 0 (先到先得)", n)
	}
	me, _ := rs.Model("hy_mt")
	if me.Provider != "hy-mt-server" {
		t.Fatalf("provider must stay hy-mt-server, got %q", me.Provider)
	}
}

// id 缺省取 map key（作者可省略 decl.id）。
func TestOuttoolIDFallsBackToKey(t *testing.T) {
	m, rs, _ := outEnv(t)
	f := &OuttoolFile{SchemaVersion: 1, Outtools: map[string]OuttoolDecl{
		"resvg": {ProvidesModels: []ProvidesModelDecl{{ID: "resvg-model", Capability: "ocr"}}},
	}}
	m.RegisterOuttools(f)
	me, ok := rs.Model("resvg-model")
	if !ok {
		t.Fatal("model should be registered")
	}
	if me.Provider != "resvg" {
		t.Fatalf("provider = %q, want resvg（id 缺省取 map key）", me.Provider)
	}
}

// 幂等：重复登记不报错、不重复计数。
func TestOuttoolRegisterIdempotent(t *testing.T) {
	m, _, _ := outEnv(t)
	f := &OuttoolFile{SchemaVersion: 1, Outtools: map[string]OuttoolDecl{
		"ollama": {ProvidesModels: []ProvidesModelDecl{{ID: "m1", Capability: "tts"}}},
	}}
	if n := m.RegisterOuttools(f); n != 1 {
		t.Fatalf("first register = %d, want 1", n)
	}
	if n := m.RegisterOuttools(f); n != 0 {
		t.Fatalf("second register = %d, want 0 (幂等)", n)
	}
}

// RegisterOuttoolsFrom 走真实文件路径；缺失即 0 且不 panic。
func TestRegisterOuttoolsFromMissingDir(t *testing.T) {
	m, _, _ := outEnv(t)
	if n := m.RegisterOuttoolsFrom(filepath.Join(t.TempDir(), "no-such-dir")); n != 0 {
		t.Fatalf("missing dir should register 0, got %d", n)
	}
}
