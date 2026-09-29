package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 现网插件 manifest 的良构样本（golden）：必须能被严格解析（§7.5 DisallowUnknownFields）。
const goldenHomeManifest = `{
  "id": "home",
  "name": "首页",
  "version": "1.0.0",
  "type": "Python",
  "python_version": "3.12",
  "entry": "main.py",
  "ui": { "type": "web", "entry": "ui/index.html" },
  "load_mode": "always",
  "permissions": ["file_read", "exec"],
  "dependencies": [],
  "codeDeps": { "runtime": { "language": "python", "version": ">=3.11" }, "deps": { "python": [] }, "lockfile": "start.lock" },
  "profiles": { "default": { "entry": "main.py", "startTimeoutMs": 5000, "transport": "stdio" } },
  "defaults": { "profile": "default", "startPolicy": { "trigger": "onAppStart" }, "stopPolicy": { "mode": "resident" } },
  "externalDependencies": [],
  "settingsSchema": { "type": "object", "properties": {} },
  "commands": [ { "name": "/clockin", "desc": "打卡", "method": "clockin" } ],
  "functions": [ { "name": "home.state", "method": "state" } ]
}`

func TestManifestStrictDecode_Golden(t *testing.T) {
	mf, err := decodeManifest([]byte(goldenHomeManifest))
	if err != nil {
		t.Fatalf("golden manifest must parse strictly, got: %v", err)
	}
	if mf.ID != "home" || mf.Version != "1.0.0" {
		t.Errorf("id/version = %q/%q, want home/1.0.0", mf.ID, mf.Version)
	}
	if mf.LoadMode != "always" {
		t.Errorf("load_mode = %q, want always", mf.LoadMode)
	}
	if len(mf.Permissions) != 2 || mf.Permissions[0] != "file_read" {
		t.Errorf("permissions = %v, want [file_read exec]", mf.Permissions)
	}
}

func TestManifestStrictDecode_RejectsUnknown(t *testing.T) {
	// §7.5 MUST：作者写了宿主不支持的字段，必须报错而非静默忽略。
	bad := strings.Replace(goldenHomeManifest, `"entry": "main.py"`, `"entry": "main.py", "vendorBogus__": 1`, 1)
	if _, err := decodeManifest([]byte(bad)); err == nil {
		t.Fatal("unknown field must be rejected under DisallowUnknownFields, got nil error")
	}
}

func TestManifestRead_RealPluginDirs(t *testing.T) {
	// 对真实插件目录做严格解析，确保开启 DisallowUnknownFields 后现网插件仍可装载。
	root := filepath.Join("..", "..", "plugins")
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("plugins dir not found (%v); skipping integration", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), "_") {
			continue
		}
		mf, err := readManifest(filepath.Join("..", ".."), d.Name())
		if err != nil {
			t.Errorf("real manifest %s must parse strictly: %v", d.Name(), err)
			continue
		}
		if mf.ID == "" {
			t.Errorf("manifest %s missing id", d.Name())
		}
	}
}
