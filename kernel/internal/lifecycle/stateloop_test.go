package lifecycle

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestStateLoopConcurrency §12.2.2：并发触发状态迁移（GetOrStart/StopPlugin/State/All）
// 由单一 state-loop goroutine 串行改写，不得死锁、不得 panic。
// 插件无可用解释器 → spawn 快速失败，反复走 STARTING→STOPPED 迁移，正好压力测试单写者。
func TestStateLoopConcurrency(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "p1")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "manifest.json"),
		[]byte(`{"id":"p1","name":"p1","type":"python","entry":"main.py"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root, nil)
	m.mu.Lock()
	m.regStore = nil // 不落盘 registry，聚焦状态机路径
	m.mu.Unlock()
	m.register(readManifestMust(t, root, "p1"))

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); _, _ = m.GetOrStart("p1") }()
		go func() { defer wg.Done(); _ = m.State("p1") }()
		go func() { defer wg.Done(); _ = m.All() }()
		go func() { defer wg.Done(); _ = m.List() }()
	}
	wg.Wait()

	// 状态机单写者：结束后读状态不应 panic。
	m.StopAll()
}

func readManifestMust(t *testing.T, root, id string) Manifest {
	t.Helper()
	mf, err := readManifest(root, id)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	return mf
}