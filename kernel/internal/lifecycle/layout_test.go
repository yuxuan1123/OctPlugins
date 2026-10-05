package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/octplugin/kernel/internal/registry"
)

// 本文件：转化域 §20.5（模型存储布局 <modelId>/<quant>/）的行为锁定。

// layoutEnv 搭一个带模型根的 Manager，并造出一个「旧布局」模型。
func layoutEnv(t *testing.T) (*Manager, *registry.Store, string) {
	t.Helper()
	m, rs, root := dlEnv(t)
	return m, rs, root
}

// 文件型旧布局模型：<root>/legacy/w.gguf
func seedLegacyFile(t *testing.T, rs *registry.Store, root, id, quant string) string {
	t.Helper()
	legacyDir := filepath.Join(root, "legacy-provider")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(legacyDir, id+".gguf")
	if err := os.WriteFile(p, []byte("weights-"+id), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rs.SetModel(id, registry.ModelEntry{
		Provider: "legacy-provider", Capability: "tts", Quant: quant, Path: p, State: "registered",
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

// 目录型旧布局模型：<root>/legacy/<id>/...
func seedLegacyDir(t *testing.T, rs *registry.Store, root, id, quant string) string {
	t.Helper()
	d := filepath.Join(root, "legacy-provider", id)
	if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"model.onnx", filepath.Join("sub", "tokens.txt")} {
		if err := os.WriteFile(filepath.Join(d, f), []byte("x-"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := rs.SetModel(id, registry.ModelEntry{
		Provider: "legacy-provider", Capability: "stt", Quant: quant, Path: d, State: "registered",
	}); err != nil {
		t.Fatal(err)
	}
	return d
}

// 审计：旧布局 → Conforming=false 且给出规范目标与原因。
func TestLayoutReportDetectsNonConforming(t *testing.T) {
	m, rs, root := layoutEnv(t)
	seedLegacyFile(t, rs, root, "hy_mt", "q4_k_m")

	rep := m.ModelLayoutReport()
	if len(rep) != 1 {
		t.Fatalf("report size = %d, want 1", len(rep))
	}
	l := rep[0]
	if l.Conforming {
		t.Fatal("legacy layout must be reported as non-conforming")
	}
	if l.Reason == "" {
		t.Fatal("non-conforming entry must explain why")
	}
	want := filepath.Join(root, "hy_mt", "q4_k_m", "hy_mt.gguf")
	if l.Canonical != want {
		t.Fatalf("canonical = %s, want %s", l.Canonical, want)
	}
	if !l.Exists || l.IsDir {
		t.Fatalf("expected existing file model, got exists=%v isDir=%v", l.Exists, l.IsDir)
	}
	if l.Bytes == 0 {
		t.Fatal("bytes should be reported for the audit")
	}
}

// 审计：已在规范布局 → Conforming=true 且无原因。
func TestLayoutReportConforming(t *testing.T) {
	m, rs, root := layoutEnv(t)
	canonDir := filepath.Join(root, "kokoro", "fp32")
	if err := os.MkdirAll(canonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(canonDir, "model.onnx")
	if err := os.WriteFile(p, []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rs.SetModel("kokoro", registry.ModelEntry{
		Provider: "sherpa-onnx", Capability: "tts", Quant: "fp32", Path: p,
	}); err != nil {
		t.Fatal(err)
	}
	l := m.ModelLayoutReport()[0]
	if !l.Conforming {
		t.Fatalf("canonical layout should conform, reason=%q", l.Reason)
	}
	if l.Reason != "" {
		t.Fatalf("conforming entry should have no reason, got %q", l.Reason)
	}
}

// dryRun 只出计划，不动文件、不改登记。
func TestRelocateDryRunDoesNotMove(t *testing.T) {
	m, rs, root := layoutEnv(t)
	src := seedLegacyFile(t, rs, root, "hy_mt", "q4_k_m")

	res, err := m.RelocateModel(context.Background(), "hy_mt", true, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Done {
		t.Fatal("dry run must not report Done")
	}
	if res.Note == "" || !strings.Contains(res.Note, "计划") {
		t.Fatalf("dry run should describe the plan, got %q", res.Note)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("dry run must not touch the source: %v", err)
	}
	if _, err := os.Stat(res.To); err == nil {
		t.Fatal("dry run must not create the target")
	}
	me, _ := rs.Model("hy_mt")
	if me.Path != src {
		t.Fatalf("dry run must not change registry path, got %s", me.Path)
	}
}

// 文件型搬迁：落到 <root>/<id>/<quant>/<文件名>，源消失，登记更新。
func TestRelocateFileModel(t *testing.T) {
	m, rs, root := layoutEnv(t)
	src := seedLegacyFile(t, rs, root, "hy_mt", "q4_k_m")

	res, err := m.RelocateModel(context.Background(), "hy_mt", false, nil)
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if !res.Done {
		t.Fatalf("expected Done, note=%q", res.Note)
	}
	want := filepath.Join(root, "hy_mt", "q4_k_m", "hy_mt.gguf")
	if res.To != want {
		t.Fatalf("to = %s, want %s", res.To, want)
	}
	got, err := os.ReadFile(want)
	if err != nil || string(got) != "weights-hy_mt" {
		t.Fatalf("moved content mismatch: %v %q", err, got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source should be gone after relocate")
	}
	me, _ := rs.Model("hy_mt")
	if me.Path != want {
		t.Fatalf("registry path = %s, want %s", me.Path, want)
	}
	// 审计应随之转为合规
	if l := m.ModelLayoutReport()[0]; !l.Conforming {
		t.Fatalf("after relocate the model should conform, reason=%q", l.Reason)
	}
}

// 目录型搬迁：内容落进 <root>/<id>/<quant>/，子目录结构保留。
func TestRelocateDirModel(t *testing.T) {
	m, rs, root := layoutEnv(t)
	src := seedLegacyDir(t, rs, root, "SenseVoiceSmall", "int8")

	res, err := m.RelocateModel(context.Background(), "SenseVoiceSmall", false, nil)
	if err != nil {
		t.Fatalf("relocate dir: %v", err)
	}
	dst := filepath.Join(root, "SenseVoiceSmall", "int8")
	if res.To != dst {
		t.Fatalf("to = %s, want %s", res.To, dst)
	}
	if res.MovedFile != 2 {
		t.Fatalf("movedFiles = %d, want 2", res.MovedFile)
	}
	for _, f := range []string{"model.onnx", filepath.Join("sub", "tokens.txt")} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Fatalf("expected %s under canonical dir: %v", f, err)
		}
	}
	if entries, _ := os.ReadDir(src); len(entries) != 0 {
		t.Fatalf("source dir should be emptied, %d entries left", len(entries))
	}
}

// 目标已存在 → 拒绝，且源保持不动（绝不覆盖既有权重）。
func TestRelocateRefusesExistingTarget(t *testing.T) {
	m, rs, root := layoutEnv(t)
	src := seedLegacyFile(t, rs, root, "hy_mt", "q4_k_m")
	dst := filepath.Join(root, "hy_mt", "q4_k_m", "hy_mt.gguf")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.RelocateModel(context.Background(), "hy_mt", false, nil); err == nil {
		t.Fatal("relocate must refuse when the target already exists")
	}
	if b, _ := os.ReadFile(dst); string(b) != "existing" {
		t.Fatal("existing target must not be overwritten")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("source must remain when relocate is refused")
	}
}

// 源在模型存储根之外 → 拒绝（安全护栏）。
func TestRelocateRefusesOutsideRoot(t *testing.T) {
	m, rs, _ := layoutEnv(t)
	outside := filepath.Join(t.TempDir(), "somewhere", "w.gguf")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rs.SetModel("outside", registry.ModelEntry{
		Provider: "p", Capability: "tts", Quant: "fp32", Path: outside,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RelocateModel(context.Background(), "outside", false, nil); err == nil {
		t.Fatal("relocate must refuse sources outside the models root")
	}
}

// 源不存在 → 报错（不静默成功）。
func TestRelocateMissingSourceFails(t *testing.T) {
	m, rs, root := layoutEnv(t)
	if err := rs.SetModel("ghost", registry.ModelEntry{
		Provider: "p", Capability: "tts", Quant: "fp32",
		Path: filepath.Join(root, "gone", "w.gguf"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RelocateModel(context.Background(), "ghost", false, nil); err == nil {
		t.Fatal("missing source must fail")
	}
}

// 未登记模型 → 报错。
func TestRelocateUnknownModelFails(t *testing.T) {
	m, _, _ := layoutEnv(t)
	if _, err := m.RelocateModel(context.Background(), "nope", false, nil); err == nil {
		t.Fatal("unknown model must fail")
	}
}

// humanBytes 的基本正确性（用于计划/进度文案）。
func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{1060000000, "1010.9 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
