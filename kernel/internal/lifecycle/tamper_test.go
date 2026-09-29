package lifecycle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/octplugin/kernel/internal/registry"
)

// TestTamperCheck §20.5：spawn 前重算 manifest hash 比对 registry，被篡改则拒绝启动。
func TestTamperCheck(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "p1")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mfPath := filepath.Join(pluginDir, "manifest.json")
	const manifest = `{"id":"p1","name":"p1","type":"python","entry":"main.py"}`
	if err := os.WriteFile(mfPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(root, nil)
	m.mu.Lock()
	m.regStore = registry.New(filepath.Join(root, "registry.json"))
	m.mu.Unlock()

	mf := Manifest{ID: "p1", Dir: pluginDir}
	m.syncRegistryEntry("p1", "1.0.0", mf) // 登记即记录 manifestHash

	if err := m.tamperCheck(mf); err != nil {
		t.Fatalf("unmodified manifest should pass tamper check: %v", err)
	}

	// 篡改 manifest 内容 → 必须拒绝启动。
	if err := os.WriteFile(mfPath, []byte(manifest+" // tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.tamperCheck(mf); err == nil {
		t.Fatal("tampered manifest must be rejected")
	}

	// 未登记基准（无 registry）：首次信赖 manifest，不拦截。
	unpinned := &Manager{}
	if err := unpinned.tamperCheck(mf); err != nil {
		t.Fatalf("no-registry path should not intercept: %v", err)
	}
}

// TestTamperCrossRestart §20.5：篡改校验基准「只首次建立」，重启（重新 syncRegistryEntry）
// 也不得重钉为篡改后的 hash，否则攻击者重启后检测即失效。
func TestTamperCrossRestart(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "p1")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mfPath := filepath.Join(pluginDir, "manifest.json")
	const manifest = `{"id":"p1","name":"p1","type":"python","entry":"main.py"}`
	if err := os.WriteFile(mfPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(root, nil)
	m.mu.Lock()
	m.regStore = registry.New(filepath.Join(root, "registry.json"))
	m.mu.Unlock()

	m.pluginsDir = root
	mf := Manifest{ID: "p1", Dir: pluginDir}

	// boot1：首次登记建立基线。
	m.syncRegistryEntry("p1", "1.0.0", mf)
	if err := m.tamperCheck(mf); err != nil {
		t.Fatalf("boot1 baseline should pass: %v", err)
	}

	// 盘上 manifest 被篡改（模拟攻击者改文件）。
	tampered := manifest + " // tampered"
	if err := os.WriteFile(mfPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	// tamperCheck 需基于真实目录重新读盘，mf.Dir 已指向 pluginDir。
	if err := m.tamperCheck(mf); err == nil {
		t.Fatal("tampered manifest must be rejected")
	}

	// boot2：重新 syncRegistryEntry 模拟重启登记——不得覆盖基线为篡改后 hash。
	m.syncRegistryEntry("p1", "1.0.0", mf)
	if err := m.tamperCheck(mf); err == nil {
		t.Fatal("after restart re-register, tampered manifest must STILL be rejected")
	}

	// 篡改 revert 回原样后应能通过（验证基线未丢而误伤）。
	if err := os.WriteFile(mfPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.tamperCheck(mf); err != nil {
		t.Fatalf("restored original manifest should pass: %v", err)
	}
}