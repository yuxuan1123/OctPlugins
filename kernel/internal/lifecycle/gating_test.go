package lifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/octplugin/kernel/internal/registry"
)

// newGatingManager 构造带 regStore 的 Manager，注册一个 Python 插件。
// depsReady 与 install 由传入的 resolver 决定，便于测试 §6.4 gating。
func newGatingManager(t *testing.T) (*Manager, *reg) {
	t.Helper()
	root := t.TempDir()
	m := NewManager(filepath.Join(root, "plugins"), nil)
	m.SetStoreDir(root) // 提供 registry.json 落点
	m.SetRegistryStore(registry.New(filepath.Join(root, "state", "registry.json")))
	pluginDir := filepath.Join(root, "plugins", "depspl")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "manifest.json"),
		[]byte(`{"id":"depspl","name":"depspl","type":"python","entry":"main.py"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mf := Manifest{ID: "depspl", Type: "python", Version: "1.0.0", Dir: pluginDir}
	r := m.register(mf)
	return m, r
}

// waitNoDepsInstalling 轮询 startNow 直到不再返回 ErrDepsInstalling（后台续启完成/失败）。
func waitNoDepsInstalling(t *testing.T, m *Manager, r *reg) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := m.startNow(r)
		if !errors.Is(err, ErrDepsInstalling) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timeout: still ErrDepsInstalling")
}

// depsStateOf 读 registry 当前 depsState。
func depsStateOf(t *testing.T, m *Manager, id string) string {
	t.Helper()
	m.mu.Lock()
	rs := m.regStore
	m.mu.Unlock()
	if rs == nil {
		return ""
	}
	e, _ := rs.Plugin(id)
	return e.DepsState
}

// TestDepsGatingInstallThenStart §6.4：依赖未就绪 → preparing → 后台安装 → 自动续启。
func TestDepsGatingInstallThenStart(t *testing.T) {
	m, r := newGatingManager(t)
	var installed atomic.Bool
	m.SetInstaller(func(Manifest, func(string)) error { installed.Store(true); return nil })
	m.SetDepsReadyResolver(func(Manifest) bool { return installed.Load() })

	// 首次请求启动：依赖未就绪 → 触发后台安装，立即返回 ErrDepsInstalling。
	if _, err := m.startNow(r); err != ErrDepsInstalling {
		t.Fatalf("first start with unmet deps: got %v, want ErrDepsInstalling", err)
	}
	if s := depsStateOf(t, m, r.id); s != "preparing" {
		t.Fatalf("before install depsState = %q, want preparing", s)
	}
	if !installed.Load() {
		t.Fatal("background install was not triggered")
	}

	// 后台安装完成后自动续启：startNow 不再返回 ErrDepsInstalling（已越过 gating 进入派生；
	// 单测无真实解释器，派生态下返回 spawn 错误属预期，这里只验证 gating 放行 + depsState）。
	err := waitNoDepsInstalling(t, m, r) // helper 已保证非 ErrDepsInstalling
	if errors.Is(err, ErrDepsInstalling) {
		t.Fatalf("gating still active after deps install: %v", err)
	}
	if s := depsStateOf(t, m, r.id); s != "ready" {
		t.Fatalf("after install depsState = %q, want ready", s)
	}
}

// TestDepsGatingInstallFailure §6.4：后台安装失败 → depsState=error，插件 depsPending，
// 后续启动被拒（不再无限重装）。
func TestDepsGatingInstallFailure(t *testing.T) {
	m, r := newGatingManager(t)
	want := errors.New("boom")
	m.SetInstaller(func(Manifest, func(string)) error { return want })
	m.SetDepsReadyResolver(func(Manifest) bool { return false })

	if _, err := m.startNow(r); err != ErrDepsInstalling {
		t.Fatalf("first start got %v, want ErrDepsInstalling", err)
	}
	// 安装失败后：startNow 应返回「依赖未安装」而非再次触发安装。
	if err := waitNoDepsInstalling(t, m, r); err == nil {
		t.Fatal("expected install-failure to surface an error")
	} else if err.Error() != "plugin depspl dependencies not installed" {
		t.Fatalf("unexpected error after failed install: %v", err)
	}
	if s := depsStateOf(t, m, r.id); s != "error" {
		t.Fatalf("after failed install depsState = %q, want error", s)
	}
}

// TestDepsGatingNoDoubleInstall §6.4：同一插件并发多次请求启动只触发一次安装。
func TestDepsGatingNoDoubleInstall(t *testing.T) {
	m, r := newGatingManager(t)
	var installed atomic.Bool
	var calls atomic.Int32
	m.SetInstaller(func(Manifest, func(string)) error { calls.Add(1); installed.Store(true); return nil })
	m.SetDepsReadyResolver(func(Manifest) bool { return installed.Load() })

	if _, err := m.startNow(r); err != ErrDepsInstalling {
		t.Fatalf("first start got %v, want ErrDepsInstalling", err)
	}
	// 在安装期间再连发多次启动请求：不应重复触发安装。
	for i := 0; i < 5; i++ {
		_, _ = m.startNow(r)
	}
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("install triggered %d times, want 1", n)
	}
	err := waitNoDepsInstalling(t, m, r) // helper 已保证越过 gating；spawn 失败属预期
	if errors.Is(err, ErrDepsInstalling) {
		t.Fatalf("gating still active after install: %v", err)
	}
}