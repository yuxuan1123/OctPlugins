package lifecycle

import "testing"

func boolp(b bool) *bool    { return &b }
func intp(i int) *int       { return &i }
func strp(s string) *string { return &s }

func TestOverrideApply(t *testing.T) {
	base := LifecyclePolicy{
		LoadMode:   LoadModePrewarm,
		Recycle:    RecycleCfg{IdleRecycle: true, MaxIdleMs: 60000},
		Resilience: ResilienceCfg{AutoRestart: boolp(true)},
	}
	ov := Override{
		LoadMode:            strp(LoadModeAlways),
		MaxIdleMs:           intp(120000),
		HeartbeatIntervalMs: intp(3000),
		HeartbeatTimeoutMs:  intp(9000),
		PrewarmPool:         intp(2),
		CrashExitCodes:      []int{0, 130},
	}
	got := ov.apply(base)
	if got.LoadMode != LoadModeAlways {
		t.Errorf("apply load_mode=%q want %q", got.LoadMode, LoadModeAlways)
	}
	if got.Recycle.IdleRecycle != true { // 未覆盖字段保留原值
		t.Errorf("apply recycle.IdleRecycle=false, want true (preserve)")
	}
	if got.Recycle.MaxIdleMs != 120000 {
		t.Errorf("apply max_idle=%d want 120000", got.Recycle.MaxIdleMs)
	}
	if got.Resilience.AutoRestart == nil || !*got.Resilience.AutoRestart {
		t.Errorf("apply should preserve auto_restart=true")
	}
	if got.Heartbeat.IntervalMs != 3000 || got.Heartbeat.TimeoutMs != 9000 {
		t.Errorf("apply heartbeat got %+v", got.Heartbeat)
	}
	if got.Start.PrewarmPool != 2 {
		t.Errorf("apply prewarm_pool=%d want 2", got.Start.PrewarmPool)
	}
	if len(got.Resilience.CrashExitCodes) != 2 || got.Resilience.CrashExitCodes[0] != 0 {
		t.Errorf("apply crash_exit_codes=%v", got.Resilience.CrashExitCodes)
	}
}

func TestOverrideHasAnyAndValidate(t *testing.T) {
	if (Override{}).hasAny() {
		t.Error("empty Override hasAny()=true, want false")
	}
	ov := Override{LoadMode: strp(LoadModeLazy)}
	if !ov.hasAny() {
		t.Error("non-empty Override hasAny()=false, want true")
	}
	if err := ValidateOverride(ov); err != nil {
		t.Errorf("ValidateOverride(lazy) err=%v want nil", err)
	}
	bad := Override{LoadMode: strp("bogus")}
	if err := ValidateOverride(bad); err == nil {
		t.Error("ValidateOverride(bogus mode) err=nil, want error")
	}
	big := Override{StdoutLineBytes: intp(9 * 1024 * 1024)}
	if err := ValidateOverride(big); err == nil {
		t.Error("ValidateOverride(9MB stdout line) err=nil, want error")
	}
}

func TestOverrideApplyResiliencePointer(t *testing.T) {
	// 显式关闭 auto_restart 应能覆盖默认开启
	base := LifecyclePolicy{Resilience: ResilienceCfg{AutoRestart: boolp(true)}}
	ov := Override{AutoRestart: boolp(false)}
	got := ov.apply(base)
	if got.Resilience.AutoRestart == nil || *got.Resilience.AutoRestart != false {
		t.Errorf("apply auto_restart=false failed, got %v", got.Resilience.AutoRestart)
	}
}
