package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/octplugin/kernel/internal/lifecycle"
)

// TestNodeReady§6.4：nodeProvider.Ready 依据 package.json 依赖声明与 node_modules 存在性判定。
func TestNodeProviderReady(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := func() lifecycle.Manifest {
		return lifecycle.Manifest{Type: "Node", Dir: dir}
	}

	// 无 package.json → 就绪
	os.Remove(filepath.Join(dir, "package.json"))
	p := nodeProvider{inst: New("", "")}
	if !p.Ready(base()) {
		t.Fatal("no package.json should be Ready")
	}

	// package.json 无第三方依赖 → 就绪（无需安装）
	mk("package.json", `{"name":"n","version":"1.0.0"}`)
	if !p.Ready(base()) {
		t.Fatal("package.json with no deps should be Ready")
	}

	// package.json 声明依赖但无 node_modules → 未就绪（触发 §6.4 安装）
	mk("package.json", `{"name":"n","dependencies":{"ms":"^2.1.3"}}`)
	if p.Ready(base()) {
		t.Fatal("deps declared but no node_modules must NOT be Ready")
	}

	// node_modules 存在 → 就绪
	os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755)
	if !p.Ready(base()) {
		t.Fatal("node_modules present should be Ready")
	}
}

// TestProviderDispatch：Install 按插件类型委托给对应 Provider（Python/Node），Go/Rust 走 noop。
func TestProviderDispatch(t *testing.T) {
	i := New("", "")
	cases := []struct {
		typ  string
		want string
	}{
		{"Python", "python"},
		{"Node", "node"},
		{"Go", "none"},
		{"Rust", "none"},
	}
	for _, c := range cases {
		p := i.providerFor(lifecycle.Manifest{Type: c.typ})
		if got := p.Lang(); got != c.want {
			t.Errorf("type %s → provider.Lang() = %q, want %q", c.typ, got, c.want)
		}
	}
}