package resources

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件：转化域 §10.1（能力维度上限）与 §11.2（graceUntil 窗口）的行为锁定。

func model(t *testing.T, m *Manager, id, capability string, companion bool, ensureErr error) *Entry {
	t.Helper()
	e := NewModel(id, "prov-"+id, capability, "fp32", "")
	e.Companion = companion
	e.SetModelHooks(ModelHooks{
		Ensure: func(context.Context) error { return ensureErr },
		Release: func() error { return nil },
	})
	return m.Register(e)
}

// §10.1：同一能力默认只允许 1 个模型同时就绪；第二个应被拒。
func TestCapabilityLimitDefaultsToOne(t *testing.T) {
	m := NewManager()
	model(t, m, "tts-a", "tts", false, nil)
	model(t, m, "tts-b", "tts", false, nil)

	if err := m.Acquire(context.Background(), "tts-a"); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	err := m.Acquire(context.Background(), "tts-b")
	if !errors.Is(err, ErrCapabilityBusy) {
		t.Fatalf("second acquire of same capability should be ErrCapabilityBusy, got %v", err)
	}
	if active, limit := m.CapabilityUsage("tts"); active != 1 || limit != 1 {
		t.Fatalf("usage = (%d,%d), want (1,1)", active, limit)
	}
}

// §10.1：不同能力互不影响。
func TestCapabilityLimitsAreIndependent(t *testing.T) {
	m := NewManager()
	model(t, m, "tts-a", "tts", false, nil)
	model(t, m, "stt-a", "stt", false, nil)
	if err := m.Acquire(context.Background(), "tts-a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(context.Background(), "stt-a"); err != nil {
		t.Fatalf("different capability must not be constrained: %v", err)
	}
}

// §11.2：窗口内上限临时为 2（快模型顶上、慢模型接班）；关闭窗口后强制回落 1。
func TestGraceWindowTemporarilyRaisesLimit(t *testing.T) {
	m := NewManager()
	model(t, m, "fast", "tts", false, nil)
	model(t, m, "slow", "tts", false, nil)
	model(t, m, "third", "tts", false, nil)

	if err := m.Acquire(context.Background(), "fast"); err != nil {
		t.Fatal(err)
	}
	if !m.InGrace("tts") {
		// 开窗前应被拒
		if err := m.Acquire(context.Background(), "slow"); !errors.Is(err, ErrCapabilityBusy) {
			t.Fatalf("pre-window acquire should be busy, got %v", err)
		}
	}
	until := m.OpenGrace("tts", 200*time.Millisecond)
	if until.IsZero() {
		t.Fatal("OpenGrace should return the deadline")
	}
	if active, limit := m.CapabilityUsage("tts"); active != 1 || limit != 2 {
		t.Fatalf("in-window usage = (%d,%d), want (1,2)", active, limit)
	}
	if err := m.Acquire(context.Background(), "slow"); err != nil {
		t.Fatalf("in-window second acquire should succeed: %v", err)
	}
	// 窗口内上限已用满，第三个仍应被拒。
	if err := m.Acquire(context.Background(), "third"); !errors.Is(err, ErrCapabilityBusy) {
		t.Fatalf("third acquire should stay busy, got %v", err)
	}
	m.CloseGrace("tts")
	if _, limit := m.CapabilityUsage("tts"); limit != 1 {
		t.Fatalf("after CloseGrace limit = %d, want 1", limit)
	}
}

// §11.2：窗口超时后自动失效（不需要显式 Close）。
func TestGraceWindowExpires(t *testing.T) {
	m := NewManager()
	m.OpenGrace("tts", 30*time.Millisecond)
	if !m.InGrace("tts") {
		t.Fatal("grace should be active right after opening")
	}
	time.Sleep(60 * time.Millisecond)
	if m.InGrace("tts") {
		t.Fatal("grace should have expired")
	}
	if _, limit := m.CapabilityUsage("tts"); limit != 1 {
		t.Fatalf("expired window must fall back to limit 1, got %d", limit)
	}
}

// §10.2：Release 归还槽位后可以换模型。
func TestReleaseFreesCapabilitySlot(t *testing.T) {
	m := NewManager()
	model(t, m, "tts-a", "tts", false, nil)
	model(t, m, "tts-b", "tts", false, nil)

	if err := m.Acquire(context.Background(), "tts-a"); err != nil {
		t.Fatal(err)
	}
	m.Release("tts-a") // 引用计数归零 → 卸载 → 归还槽位
	if active, _ := m.CapabilityUsage("tts"); active != 0 {
		t.Fatalf("after release active = %d, want 0", active)
	}
	if err := m.Acquire(context.Background(), "tts-b"); err != nil {
		t.Fatalf("acquire after release should succeed: %v", err)
	}
}

// 伴随模型（det+rec / codec 依赖）不占能力维度上限，可与主模型并存。
func TestCompanionDoesNotConsumeCapabilitySlot(t *testing.T) {
	m := NewManager()
	model(t, m, "rapidocr", "ocr", false, nil)
	model(t, m, "rapidocr-rec", "ocr", true, nil)

	if err := m.Acquire(context.Background(), "rapidocr"); err != nil {
		t.Fatal(err)
	}
	if err := m.Acquire(context.Background(), "rapidocr-rec"); err != nil {
		t.Fatalf("companion model must not be blocked by the capability cap: %v", err)
	}
	if active, limit := m.CapabilityUsage("ocr"); active != 1 || limit != 1 {
		t.Fatalf("companion must not count: usage = (%d,%d), want (1,1)", active, limit)
	}
}

// 加载失败必须归还预留，不能把能力槽位永久占死。
func TestFailedLoadReleasesReservation(t *testing.T) {
	m := NewManager()
	model(t, m, "tts-bad", "tts", false, errors.New("weights missing"))
	model(t, m, "tts-ok", "tts", false, nil)

	if err := m.Acquire(context.Background(), "tts-bad"); err == nil {
		t.Fatal("expected acquire failure")
	}
	if e := m.Get("tts-bad"); e == nil || e.State != "failed" {
		t.Fatalf("failed model state = %v, want failed", m.Get("tts-bad").State)
	}
	if active, _ := m.CapabilityUsage("tts"); active != 0 {
		t.Fatalf("failed load must not hold a slot, active = %d", active)
	}
	if err := m.Acquire(context.Background(), "tts-ok"); err != nil {
		t.Fatalf("slot should be free after a failed load: %v", err)
	}
}

// §11.2：Entry.GraceUntil 作为外部可见的镜像字段。
func TestGraceUntilMirror(t *testing.T) {
	m := NewManager()
	e := model(t, m, "tts-a", "tts", false, nil)
	if !e.GraceUntilAt().IsZero() {
		t.Fatal("grace should start zero")
	}
	until := time.Now().Add(time.Second)
	e.SetGraceUntil(until)
	if got := e.GraceUntilAt(); !got.Equal(until) {
		t.Fatalf("GraceUntilAt = %v, want %v", got, until)
	}
}
