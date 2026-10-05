package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/octplugin/kernel/internal/perms"
	"github.com/octplugin/kernel/internal/registry"
	"github.com/octplugin/kernel/internal/resources"
)

// 本文件：转化域 §26（首次下载由宿主执行）的行为锁定。
// 用 httptest 本地服务，不触网、不下真实权重。

func dlEnv(t *testing.T) (*Manager, *registry.Store, string) {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(dir, perms.NewGate(filepath.Join(dir, "perms.json")))
	rs := registry.New(filepath.Join(dir, "registry.json"))
	m.SetRegistryStore(rs)
	m.SetResourceManager(resources.NewManager())
	root := filepath.Join(dir, "modelsRoot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	m.SetModelsRoot(root)
	return m, rs, root
}

// 未声明 downloadUrl → 明确拒绝并给出人工放置路径（不伪造可下载）。
func TestPullWithoutDownloadURLRefused(t *testing.T) {
	m, rs, _ := dlEnv(t)
	if err := rs.SetModel("m1", registry.ModelEntry{
		Provider: "t", Capability: "tts", Path: "m1/fp32/w.bin",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.PullModel(context.Background(), "m1", nil)
	if err == nil {
		t.Fatal("model without downloadUrl must be refused")
	}
	if !strings.Contains(err.Error(), "downloadUrl") {
		t.Fatalf("error should explain the missing downloadUrl: %v", err)
	}
}

// 正常下载：落到 <modelsRoot>/<modelId>/<quant>/，并更新 registry.path（§20.5）。
func TestPullModelDownloadsAndRegisters(t *testing.T) {
	payload := []byte("fake-weights-payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "20")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m, rs, root := dlEnv(t)
	if err := rs.SetModel("m1", registry.ModelEntry{
		Provider: "t", Capability: "tts", Quant: "fp32",
		Path: "m1/fp32/legacy.bin", DownloadURL: srv.URL + "/weights.bin",
	}); err != nil {
		t.Fatal(err)
	}

	var phases []string
	dest, err := m.PullModel(context.Background(), "m1", func(p DownloadProgress) {
		phases = append(phases, p.Phase)
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	want := filepath.Join(root, "m1", "fp32", "weights.bin")
	if dest != want {
		t.Fatalf("dest = %s, want %s（§20.5 布局）", dest, want)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("downloaded content mismatch: %v %q", err, got)
	}
	// 中途不得留下 .part
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part temp file should be renamed away on success")
	}
	// registry.path 指向新产物，后续 Acquire 直接用
	me, _ := rs.Model("m1")
	if me.Path != dest {
		t.Fatalf("registry path = %s, want %s", me.Path, dest)
	}
	// 进度序列应含 started 与终态 done
	if len(phases) < 2 || phases[0] != "started" || phases[len(phases)-1] != "done" {
		t.Fatalf("unexpected phase sequence: %v", phases)
	}
}

// 声明了 downloadSha256 但不匹配 → 丢弃产物、报错、不留 .part、不改 registry。
func TestPullModelSha256MismatchDiscards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered-payload"))
	}))
	defer srv.Close()

	m, rs, root := dlEnv(t)
	if err := rs.SetModel("m1", registry.ModelEntry{
		Provider: "t", Capability: "tts", Quant: "fp32",
		Path: "m1/fp32/w.bin", DownloadURL: srv.URL + "/w.bin",
		DownloadSHA256: strings.Repeat("ab", 32), // 必然不匹配
	}); err != nil {
		t.Fatal(err)
	}
	var failed DownloadProgress
	_, err := m.PullModel(context.Background(), "m1", func(p DownloadProgress) {
		if p.Phase == "failed" {
			failed = p
		}
	})
	if err == nil {
		t.Fatal("sha256 mismatch must fail")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("error should mention sha256: %v", err)
	}
	if failed.Phase != "failed" {
		t.Fatalf("expected a failed progress event, got %+v", failed)
	}
	destDir := filepath.Join(root, "m1", "fp32")
	entries, _ := os.ReadDir(destDir)
	for _, e := range entries {
		t.Fatalf("产物应被丢弃，但残留 %s", e.Name())
	}
	me, _ := rs.Model("m1")
	if me.Path != "m1/fp32/w.bin" {
		t.Fatalf("registry path must stay unchanged on failure, got %s", me.Path)
	}
}

// sha256 匹配 → 通过。
func TestPullModelSha256MatchSucceeds(t *testing.T) {
	payload := []byte("good-payload")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m, rs, _ := dlEnv(t)
	if err := rs.SetModel("m1", registry.ModelEntry{
		Provider: "t", Capability: "tts", Quant: "int8",
		Path: "m1/int8/w.bin", DownloadURL: srv.URL + "/w.bin",
		DownloadSHA256: hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatal(err)
	}
	dest, err := m.PullModel(context.Background(), "m1", nil)
	if err != nil {
		t.Fatalf("pull with matching sha256 should succeed: %v", err)
	}
	if !strings.Contains(dest, filepath.Join("m1", "int8")) {
		t.Fatalf("dest should follow <modelId>/<quant> layout: %s", dest)
	}
}

// HTTP 非 200 → 失败且不落盘。
func TestPullModelHTTPErrorFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	m, rs, _ := dlEnv(t)
	if err := rs.SetModel("m1", registry.ModelEntry{
		Provider: "t", Capability: "tts", Path: "m1/w.bin", DownloadURL: srv.URL + "/w.bin",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PullModel(context.Background(), "m1", nil); err == nil {
		t.Fatal("HTTP 404 must fail")
	}
}

// 未登记的模型 → 明确报错。
func TestPullUnknownModelFails(t *testing.T) {
	m, _, _ := dlEnv(t)
	if _, err := m.PullModel(context.Background(), "nope", nil); err == nil {
		t.Fatal("unknown model must fail")
	}
}

// downloadFileName 的边界：带 query、带尾斜杠、无路径段。
// 取 URL 末段作为文件名；末段不可用时（空 / . / ..）退回 <modelId>.bin。
func TestDownloadFileName(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://x/y/weights.bin", "weights.bin"},
		{"https://x/y/weights.bin?token=abc", "weights.bin"},
		{"https://x/y/", "y"},
		{"https://x/y", "y"},
		{"https://x/", "x"},
	}
	for _, c := range cases {
		if got := downloadFileName(c.url, "m1"); got != c.want {
			t.Fatalf("downloadFileName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
