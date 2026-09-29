package lifecycle

import (
	"encoding/json"
	"strings"
	"testing"
)

// manifestJSON 一个旧式 manifest（不含任何生命周期字段）→ 必须向后兼容自动填默认。
const oldManifestJSON = `{
	"id": "demo", "name": "Demo", "type": "python", "entry": "main.py",
	"ui_mode": "web",
	"permissions": ["file.read", "net"]
}`

func TestLifecycleDefaults_BackwardCompat(t *testing.T) {
	var mf Manifest
	if err := json.Unmarshal([]byte(oldManifestJSON), &mf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mf.LifecyclePolicy.ApplyDefaults()

	if mf.LoadMode != DefaultLoadMode {
		t.Errorf("load_mode default = %q, want %q", mf.LoadMode, DefaultLoadMode)
	}
	if mf.Heartbeat.IntervalMs != DefaultHeartbeatIntervalMs {
		t.Errorf("heartbeat.interval_ms default = %d, want %d", mf.Heartbeat.IntervalMs, DefaultHeartbeatIntervalMs)
	}
	if mf.Heartbeat.TimeoutMs != DefaultHeartbeatTimeoutMs {
		t.Errorf("heartbeat.timeout_ms default = %d, want %d", mf.Heartbeat.TimeoutMs, DefaultHeartbeatTimeoutMs)
	}
	if mf.Resilience.AutoRestart == nil || *mf.Resilience.AutoRestart != DefaultAutoRestart {
		t.Errorf("auto_restart default = %v, want %v (true)", mf.Resilience.AutoRestart, DefaultAutoRestart)
	}
	if mf.Limits.StdoutLineBytes != DefaultStdoutLineBytes {
		t.Errorf("stdout_line_bytes default = %d, want %d", mf.Limits.StdoutLineBytes, DefaultStdoutLineBytes)
	}
	if err := mf.LifecyclePolicy.Validate(); err != nil {
		t.Errorf("validated defaulted manifest should pass, got: %v", err)
	}
}

func TestLifecycleValidate(t *testing.T) {
	cases := []struct {
		name   string
		policy LifecyclePolicy
		wantOK bool
	}{
		{"defaulted valid", LifecyclePolicy{LoadMode: "always", Heartbeat: HeartbeatCfg{IntervalMs: 5, TimeoutMs: 15}, Recycle: RecycleCfg{IdleRecycle: true, MaxIdleMs: 100}, Resilience: ResilienceCfg{AutoRestart: boolPtr(true), BackoffBaseMs: 5, BackoffMaxMs: 10}, Limits: LimitsCfg{StdoutLineBytes: 8 * 1024 * 1024}}, true},
		{"bad load_mode", LifecyclePolicy{LoadMode: "forever"}, false},
		{"timeout < interval", LifecyclePolicy{LoadMode: "lazy", Heartbeat: HeartbeatCfg{IntervalMs: 20, TimeoutMs: 10}}, false},
		{"idle recycle w/o max", LifecyclePolicy{LoadMode: "lazy", Recycle: RecycleCfg{IdleRecycle: true}}, false},
		{"backoff max < base", LifecyclePolicy{LoadMode: "lazy", Resilience: ResilienceCfg{AutoRestart: boolPtr(true), BackoffBaseMs: 10, BackoffMaxMs: 5}}, false},
		{"stdout over hard limit", LifecyclePolicy{LoadMode: "lazy", Limits: LimitsCfg{StdoutLineBytes: 9 * 1024 * 1024}}, false},
		{"explicit auto_restart false ok", LifecyclePolicy{LoadMode: "lazy", Resilience: ResilienceCfg{AutoRestart: boolPtr(false), BackoffBaseMs: 5, BackoffMaxMs: 10}}, true},
	}
	for _, c := range cases {
		p := c.policy
		p.ApplyDefaults() // 让非必填项有默认，只测目标项
		err := p.Validate()
		if c.wantOK && err != nil {
			t.Errorf("%s: want ok, got %v", c.name, err)
		}
		if !c.wantOK && err == nil {
			t.Errorf("%s: want error, got ok", c.name)
		}
	}
}

func TestLifecycleHasTriggers(t *testing.T) {
	empty := LifecyclePolicy{}
	if empty.HasTriggers() {
		t.Error("empty policy should not have triggers")
	}
	p := LifecyclePolicy{LoadMode: "lazy", Triggers: Triggers{OnCommand: []string{"/md"}, OnFileExt: []string{".md"}}}
	if !p.HasTriggers() {
		t.Error("policy with triggers should report HasTriggers=true")
	}
	if !strings.EqualFold("prewarm", DefaultLoadMode) {
		t.Error("default load_mode should be prewarm")
	}
}
