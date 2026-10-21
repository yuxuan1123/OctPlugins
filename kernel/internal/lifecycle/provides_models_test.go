package lifecycle

import (
	"testing"

	"github.com/octplugin/kernel/internal/resources"
)

// 本文件：转化域 §三/§14.5 —— 提供模型的插件自身必须能被 ResourceMap 查到。
//
// 回归背景：模型条目的 backend 是「提供它的插件 id」（registerProvidedModels 用 mf.ID），
// 模型 Acquire 前会先 Acquire(backend)。若该插件本身没进 ResourceMap，每次模型就绪都会打
// `[resources] model X backend Y acquire degraded: resource "Y" not registered`（§14.5 查表落空）。

func TestProvidedModelBackendToolIsRegistered(t *testing.T) {
	m, rs, _ := outEnv(t)
	mf := Manifest{ID: "prov-x"}
	mf.ProvidesModels = []ProvidesModelDecl{
		{ID: "m1", Capability: "translate", Backend: "ctranslate2", Path: "x", Quant: "int8"},
	}
	m.registerProvidedModels(mf)

	rm := m.resourceManager()
	e := rm.Get("prov-x")
	if e == nil || e.Kind != resources.KindTool {
		t.Fatalf("provider plugin must be registered as kind=tool, got %+v", e)
	}
	mod := rm.Get("m1")
	if mod == nil {
		t.Fatal("model should be registered in ResourceMap")
	}
	if mod.Backend != "prov-x" {
		t.Fatalf("model backend = %q, want prov-x", mod.Backend)
	}
	if _, ok := rs.Model("m1"); !ok {
		t.Fatal("model should be registered in registry.json models")
	}

	// 卸载插件时连带摘除自动登记的 tool 条目（避免残留悬空 backend）。
	m.unregisterProvidedModels("prov-x")
	if rm.Get("prov-x") != nil {
		t.Fatal("auto-registered provider tool should be removed on unregister")
	}
}

// 外部已声明的同 id tool 不被覆盖：注入的定位方案要保持原样。
func TestProviderToolDoesNotOverrideDeclaredTool(t *testing.T) {
	m, _, _ := outEnv(t)
	rm := m.resourceManager()
	declared := resources.NewTool("prov-y", resources.LocateExplicitPath, "/opt/prov-y", "")
	rm.Register(declared)

	m.registerProviderTool("prov-y")
	if got := rm.Get("prov-y"); got != declared {
		t.Fatal("existing tool entry must be reused, not replaced")
	}
}
