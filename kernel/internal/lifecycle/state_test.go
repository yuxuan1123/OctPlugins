package lifecycle

import (
	"testing"
	"time"
)

func boolT(b bool) *bool { return &b }

func TestRegStateString(t *testing.T) {
	cases := map[RegState]string{
		RegRegistered: "REGISTERED", RegStarting: "STARTING", RegRunning: "RUNNING",
		RegIdle: "IDLE", RegStopped: "STOPPED",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("state %d String()=%q want %q", s, got, want)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	m := &Manager{}
	r := &reg{mf: Manifest{LifecyclePolicy: LifecyclePolicy{
		Resilience: ResilienceCfg{
			AutoRestart: boolT(true), MaxRestarts: 2, MaxRestartsWindowMs: 60000,
			BackoffBaseMs: 10, BackoffMaxMs: 80,
		},
	}}}

	// attempt0→base=10；attempt1→20；第三次达到 max=2 → 暂停。
	if d, ok := m.nextBackoffLocked(r, 0); !ok || d != 10*time.Millisecond {
		t.Errorf("attempt0: got %v/%v", d, ok)
	}
	if d, ok := m.nextBackoffLocked(r, 1); !ok || d != 20*time.Millisecond {
		t.Errorf("attempt1: got %v/%v", d, ok)
	}
	if _, ok := m.nextBackoffLocked(r, 2); ok {
		t.Error("attempt2 (>= max) should be disallowed")
	}

	// 滑动窗口：window 外的时间戳被裁剪。塞一个过期时间戳，应被剪掉并重新允许。
	r.crash = []time.Time{time.Now().Add(-2 * time.Minute)}
	if _, ok := m.nextBackoffLocked(r, 3); !ok {
		t.Error("expired crash timestamp should be pruned and allow a retry")
	}
}

func TestNextBackoffDisabledOrNormalExit(t *testing.T) {
	m := &Manager{}
	r := &reg{mf: Manifest{LifecyclePolicy: LifecyclePolicy{
		Resilience: ResilienceCfg{AutoRestart: boolT(false)},
	}}}
	if _, ok := m.nextBackoffLocked(r, 1); ok {
		t.Error("auto_restart=false should disable restart")
	}

	r = &reg{mf: Manifest{LifecyclePolicy: LifecyclePolicy{
		Resilience: ResilienceCfg{AutoRestart: boolT(true), CrashExitCodes: []int{0, 130}},
	}}}
	if _, ok := m.nextBackoffLocked(r, 0); ok {
		t.Error("exit code in crash_exit_codes should be normal exit, no restart")
	}
}

func TestRecycleDue(t *testing.T) {
	m := &Manager{}
	newReg := func(always, idle, bg bool, maxIdle int) *reg {
		lm := LoadModeLazy
		if always {
			lm = LoadModeAlways
		}
		return &reg{mf: Manifest{LifecyclePolicy: LifecyclePolicy{
			LoadMode: lm,
			Recycle:  RecycleCfg{IdleRecycle: idle, MaxIdleMs: maxIdle, BackgroundTasks: bg},
		}}}
	}
	// 零值 Plugin：LastActivity=零时刻 → 远早于阈值 → 判定空闲。
	zeroP := &Plugin{}
	if !m.recycleDue(newReg(false, true, false, 100), zeroP) {
		t.Error("lazy + idle_recycle + long idle should be recyclable")
	}
	if m.recycleDue(newReg(true, true, false, 100), zeroP) {
		t.Error("always plugin must never be idle-recycled")
	}
	if m.recycleDue(newReg(false, true, true, 100), zeroP) {
		t.Error("background_tasks=true should skip idle recycle")
	}
	if m.recycleDue(newReg(false, true, false, 0), zeroP) {
		t.Error("max_idle_ms<=0 should be disabled")
	}
	if m.recycleDue(newReg(false, false, false, 100), zeroP) {
		t.Error("idle_recycle=false should be disabled")
	}
}
