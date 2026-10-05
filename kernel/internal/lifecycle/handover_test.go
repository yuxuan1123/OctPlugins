package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/octplugin/kernel/internal/perms"
	"github.com/octplugin/kernel/internal/registry"
	"github.com/octplugin/kernel/internal/resources"
)

// 本文件：转化域 §12（handover 流程）与 §13（窗口内的资源账）的行为锁定。

// hoEnv 搭一个只含 ResourceMap + registry 的 Manager，用于模型编排测试。
type hoEnv struct {
	m  *Manager
	rm *resources.Manager
	rs *registry.Store
}

func newHOEnv(t *testing.T) *hoEnv {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(dir, perms.NewGate(filepath.Join(dir, "perms.json")))
	rs := registry.New(filepath.Join(dir, "registry.json"))
	m.SetRegistryStore(rs)
	rm := resources.NewManager()
	m.SetResourceManager(rm)
	return &hoEnv{m: m, rm: rm, rs: rs}
}

// add 登记一个模型：先写 registry（声明元信息），再注册进 ResourceMap（加载钩子）。
func (h *hoEnv) add(t *testing.T, id, capability string, minVram float64, coldStartMs int, ensureErr error) {
	t.Helper()
	if _, ok := h.rs.Model(id); !ok {
		if err := h.rs.SetModel(id, registry.ModelEntry{
			Provider: "prov", Capability: capability, Backend: "prov",
			MinVramGB: minVram, ColdStartMs: coldStartMs, State: "registered",
		}); err != nil {
			t.Fatalf("SetModel(%s): %v", id, err)
		}
	}
	e := resources.NewModel(id, "prov", capability, "fp32", "")
	e.SetModelHooks(resources.ModelHooks{
		Ensure:  func(context.Context) error { return ensureErr },
		Release: func() error { return nil },
	})
	h.rm.Register(e)
}

// §12.1–§12.3：有快模型在服务时切慢模型 → 窗口模式并行接班，成功后就绪并释放快模型。
func TestHandoverWindowSucceedsAndReleasesFastModel(t *testing.T) {
	h := newHOEnv(t)
	h.add(t, "fast", "tts", 0, 1000, nil)
	h.add(t, "slow", "tts", 0, 5000, nil)

	if err := h.rm.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.SwitchModelSync(context.Background(), "tts", "slow")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if st.Mode != "window" {
		t.Fatalf("mode = %q, want window", st.Mode)
	}
	if st.From != "fast" || st.To != "slow" {
		t.Fatalf("from/to = %q/%q, want fast/slow", st.From, st.To)
	}
	if st.Phase != "ready" {
		t.Fatalf("phase = %q (%s), want ready", st.Phase, st.Reason)
	}
	if e := h.rm.Get("slow"); e == nil || e.State != "ready" {
		t.Fatalf("slow model state = %v, want ready", e.State)
	}
	// §12.3：接班后 Release 快模型。
	if e := h.rm.Get("fast"); e != nil && e.State == "ready" {
		t.Fatalf("fast model should have been released, state = %q", e.State)
	}
	// §11.2：窗口关闭，上限回落 1。
	if _, limit := h.rm.CapabilityUsage("tts"); limit != 1 {
		t.Fatalf("limit after handover = %d, want 1", limit)
	}
}

// §12.4：慢模型加载失败 → 快模型继续服务，降级不停工。
func TestHandoverFailureKeepsFastModelServing(t *testing.T) {
	h := newHOEnv(t)
	h.add(t, "fast", "tts", 0, 1000, nil)
	h.add(t, "slow", "tts", 0, 5000, errors.New("OOM while loading weights"))

	if err := h.rm.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.SwitchModelSync(context.Background(), "tts", "slow")
	if err != nil {
		t.Fatalf("switch should not return a hard error on load failure: %v", err)
	}
	if st.Phase != "failed" {
		t.Fatalf("phase = %q, want failed", st.Phase)
	}
	if st.Reason == "" {
		t.Fatal("failure must carry a user-facing reason")
	}
	if e := h.rm.Get("fast"); e == nil || e.State != "ready" {
		t.Fatalf("fast model must keep serving, state = %v", e.State)
	}
	if active, _ := h.rm.CapabilityUsage("tts"); active != 1 {
		t.Fatalf("active = %d, want 1 (fast still serving)", active)
	}
}

// §13.1/§13.2：并存期显存不足 → 放弃 handover，改串行切换（先 Release 再 Acquire）。
func TestHandoverFallsBackToSerialWhenVramInsufficient(t *testing.T) {
	t.Setenv("OCT_VRAM_GB", "2")
	h := newHOEnv(t)
	h.add(t, "fast", "tts", 1.5, 1000, nil)
	h.add(t, "slow", "tts", 1.5, 5000, nil)

	if err := h.rm.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.SwitchModelSync(context.Background(), "tts", "slow")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if st.Mode != "serial" {
		t.Fatalf("mode = %q, want serial", st.Mode)
	}
	if st.NeedVramGB != 3 {
		t.Fatalf("needVramGB = %v, want 3 (1.5+1.5)", st.NeedVramGB)
	}
	if st.Phase != "ready" {
		t.Fatalf("phase = %q (%s), want ready", st.Phase, st.Reason)
	}
	if e := h.rm.Get("slow"); e == nil || e.State != "ready" {
		t.Fatal("slow model should be loaded after serial switch")
	}
}

// §13.2/§15.5：需要显存但预算未知 → MUST NOT 猜测，改串行切换并说明原因。
func TestHandoverSerialWhenVramUnknown(t *testing.T) {
	os.Unsetenv("OCT_VRAM_GB")
	h := newHOEnv(t)
	h.add(t, "fast", "tts", 1.0, 1000, nil)
	h.add(t, "slow", "tts", 1.0, 5000, nil)

	if err := h.rm.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.SwitchModelSync(context.Background(), "tts", "slow")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != "serial" {
		t.Fatalf("mode = %q, want serial when VRAM budget is unknown", st.Mode)
	}
	if !contains(st.Reason, "MUST NOT") && !contains(st.Reason, "未知") {
		t.Fatalf("reason should explain the unknown budget: %q", st.Reason)
	}
}

// 纯 CPU 模型（minVramGB=0）不受显存预算限制，仍走并行窗口。
func TestCpuOnlyModelsAlwaysUseWindow(t *testing.T) {
	os.Unsetenv("OCT_VRAM_GB")
	h := newHOEnv(t)
	h.add(t, "fast", "ocr", 0, 1500, nil)
	h.add(t, "slow", "ocr", 0, 1500, nil)

	if err := h.rm.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.SwitchModelSync(context.Background(), "ocr", "slow")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != "window" {
		t.Fatalf("mode = %q, want window for CPU-only models", st.Mode)
	}
}

// §12：切到已在服务的模型是空操作。
func TestSwitchToCurrentModelIsNoop(t *testing.T) {
	h := newHOEnv(t)
	h.add(t, "fast", "tts", 0, 1000, nil)
	if err := h.rm.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.SwitchModelSync(context.Background(), "tts", "fast")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != "noop" || st.Phase != "ready" {
		t.Fatalf("got mode=%q phase=%q, want noop/ready", st.Mode, st.Phase)
	}
}

// §12.2：EnsureModel 在已有快模型时立即返回可用模型，并把接班模型标为 pending。
func TestEnsureModelReturnsFastModelWhileLoadingSlow(t *testing.T) {
	h := newHOEnv(t)
	h.add(t, "kokoro", "tts", 0, 5000, nil)
	h.add(t, "moss", "tts", 0, 15000, nil)
	// 让 kokoro 成为作者默认（§7.2 要求 tts 默认是 Kokoro）。
	mustDefault(t, h.rs, "kokoro")

	if err := h.rm.Acquire(context.Background(), "kokoro"); err != nil {
		t.Fatal(err)
	}
	res, err := h.m.EnsureModel(context.Background(), "tts", "")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.ModelID != "kokoro" || res.State != "ready" {
		t.Fatalf("already-ready default should return immediately, got %+v", res)
	}

	// 任务级 override 切 MOSS：首任务仍应由 kokoro 顶上（§12.1）。
	res, err = h.m.EnsureModel(context.Background(), "tts", "moss")
	if err != nil {
		t.Fatalf("ensure override: %v", err)
	}
	if res.Source != "taskOverride" {
		t.Fatalf("source = %q, want taskOverride", res.Source)
	}
	if res.ModelID != "kokoro" {
		t.Fatalf("first task should keep using the fast model, got %q", res.ModelID)
	}
	if res.Pending != "moss" || res.State != "loading" {
		t.Fatalf("expected pending moss/loading, got state=%q pending=%q", res.State, res.Pending)
	}
	if !contains(res.Note, "正在加载") {
		t.Fatalf("note should be user-facing (§12.2): %q", res.Note)
	}
}

func mustDefault(t *testing.T, rs *registry.Store, id string) {
	t.Helper()
	me, ok := rs.Model(id)
	if !ok {
		t.Fatalf("model %s not registered", id)
	}
	me.Default = true
	if err := rs.SetModel(id, me); err != nil {
		t.Fatal(err)
	}
}

// §11.1 + §12：窗口按目标模型声明的 coldStartMs 自适应。
// 基础窗口 10s 是下限；冷启动更慢的模型得到更长的窗口，否则 §12 对它们永远超时。
func TestHandoverWindowAdaptsToColdStart(t *testing.T) {
	os.Unsetenv("OCT_HANDOVER_WINDOW_MS")

	if got := handoverWindowFor(0); got != DefaultHandoverWindowMs*time.Millisecond {
		t.Fatalf("no coldStart → base window, got %s", got)
	}
	// 快模型：coldStart 3s + 5s 余量 = 8s < 10s → 仍用 10s 下限
	if got := handoverWindowFor(3000); got != DefaultHandoverWindowMs*time.Millisecond {
		t.Fatalf("fast model should keep the 10s floor, got %s", got)
	}
	// 慢模型：coldStart 15s（本机 MOSS/hy_mt）→ 20s > 10s → 放宽
	if got := handoverWindowFor(15000); got != 20*time.Second {
		t.Fatalf("slow model should widen to 20s, got %s", got)
	}
	// 荒谬值被上限截断
	if got := handoverWindowFor(10 * 60 * 1000); got != maxHandoverWindowMs*time.Millisecond {
		t.Fatalf("absurd coldStart should be capped, got %s", got)
	}
	// 环境变量仍可抬高下限
	t.Setenv("OCT_HANDOVER_WINDOW_MS", "60000")
	if got := handoverWindowFor(15000); got != 60*time.Second {
		t.Fatalf("env override should raise the floor, got %s", got)
	}
}

// §12.2/§12.4：慢模型（coldStart 15s > 10s）接班时窗口被放宽，不再必然超时。
func TestSlowModelHandoverGetsWidenedWindow(t *testing.T) {
	os.Unsetenv("OCT_HANDOVER_WINDOW_MS")
	h := newHOEnv(t)
	h.add(t, "fast", "tts", 0, 1000, nil)
	h.add(t, "slow", "tts", 0, 15000, nil)

	if err := h.rm.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	st, err := h.m.SwitchModelSync(context.Background(), "tts", "slow")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != "window" {
		t.Fatalf("mode = %q, want window", st.Mode)
	}
	// 核心断言：窗口 > 10s（否则 15s 冷启动的模型永远接不了班）
	granted := time.Until(st.Deadline)
	if granted <= DefaultHandoverWindowMs*time.Millisecond {
		t.Fatalf("window for a 15s-coldStart model should exceed 10s, got %s", granted)
	}
	if st.Phase != "ready" {
		t.Fatalf("phase = %q (%s), want ready", st.Phase, st.Reason)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = time.Second
