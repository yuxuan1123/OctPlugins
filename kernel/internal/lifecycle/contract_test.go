package lifecycle

import (
	"strings"
	"testing"

	"github.com/octplugin/kernel/internal/perms"
)

// 本文件：转化域 §23（conversion manifest 契约）的行为锁定。

func contractManifest() Manifest {
	return Manifest{
		ID: "conversion",
		ManifestFields: ManifestFields{
			Permissions:          []string{perms.FileRead, perms.FileWrite, perms.LocalModel, perms.Network},
			RequiresCapabilities: []string{"ocr", "stt", "tts", "translate"},
			ExternalDeps: []ExternalDep{
				{ID: "pandoc", Kind: "tool", Optional: true},
				{ID: "soffice", Kind: "tool", Optional: true},
				{ID: "ffmpeg", Kind: "tool", Optional: true},
				{ID: "resvg", Kind: "tool", Optional: true},
			},
			Spawn: []string{"pandoc", "soffice", "ffmpeg", "resvg"},
			Net:   false,
			Profiles: map[string]ProfileDecl{
				"default": {Desc: "本地转换"},
				"m3u8":    {Optional: true, Net: boolPtr(true), Desc: "远程拉取"},
			},
		},
	}
}

// §23 合规的 conversion manifest 应通过校验。
func TestContractValid(t *testing.T) {
	if err := validateManifestContract(contractManifest()); err != nil {
		t.Fatalf("valid contract should pass: %v", err)
	}
}

// §23.2：spawn 白名单里的 id 必须是已声明的 outtool。
func TestContractSpawnUnknownOuttoolRejected(t *testing.T) {
	mf := contractManifest()
	mf.Spawn = append(mf.Spawn, "imagemagick")
	err := validateManifestContract(mf)
	if err == nil {
		t.Fatal("spawn entry not in externalDependencies must be rejected")
	}
	if !strings.Contains(err.Error(), "imagemagick") {
		t.Fatalf("error should name the offending id: %v", err)
	}
}

// §23.2：net=true 必须声明 network 权限。
func TestContractNetWithoutPermissionRejected(t *testing.T) {
	mf := contractManifest()
	mf.Permissions = []string{perms.FileRead, perms.FileWrite, perms.LocalModel}
	err := validateManifestContract(mf)
	if err == nil {
		t.Fatal("net=true without network permission must be rejected")
	}
}

// §23.3：profile 级 net=true 同样要求 network 权限。
func TestContractProfileNetWithoutPermissionRejected(t *testing.T) {
	mf := contractManifest()
	mf.Permissions = []string{perms.FileRead, perms.FileWrite, perms.LocalModel}
	mf.Net = false // manifest 级关闭，仅 profile 打开
	err := validateManifestContract(mf)
	if err == nil {
		t.Fatal("profile net=true without network permission must be rejected")
	}
}

// net=false 且未声明 network 权限 → 合法（默认离线）。
func TestContractOfflineWithoutPermissionOk(t *testing.T) {
	mf := contractManifest()
	mf.Permissions = []string{perms.FileRead, perms.FileWrite}
	mf.Profiles = map[string]ProfileDecl{"default": {}}
	if err := validateManifestContract(mf); err != nil {
		t.Fatalf("offline manifest should pass: %v", err)
	}
}

// §23.1：requiresCapabilities 里的能力被声明 → 允许请求。
func TestRequiresCapabilityAllowsDeclared(t *testing.T) {
	mf := contractManifest()
	for _, c := range []string{"ocr", "stt", "tts", "translate"} {
		hasList, ok := requiresCapability(mf, c)
		if !hasList || !ok {
			t.Fatalf("declared capability %q should be allowed", c)
		}
	}
}

// §23.1：未声明的能力 → 拒绝（防止插件越权索取资源）。
func TestRequiresCapabilityRejectsUndeclared(t *testing.T) {
	mf := contractManifest()
	hasList, ok := requiresCapability(mf, "video-upscale")
	if !hasList {
		t.Fatal("manifest declares requiresCapabilities, so the list applies")
	}
	if ok {
		t.Fatal("undeclared capability must be rejected")
	}
}

// 未声明 requiresCapabilities 的插件不受该白名单约束（向后兼容）。
func TestRequiresCapabilityNoDeclarationIsOpen(t *testing.T) {
	mf := Manifest{ID: "legacy-tool"}
	hasList, ok := requiresCapability(mf, "anything")
	if hasList {
		t.Fatal("no declaration means no whitelist")
	}
	if !ok {
		t.Fatal("no declaration should not block")
	}
}

// §23.3：profile 的 net 生效顺序为 profile 显式 > manifest 缺省。
func TestEffectiveNetPrecedence(t *testing.T) {
	mf := contractManifest()
	if effectiveNet(mf, "default") {
		t.Fatal("default profile should be offline (manifest net=false)")
	}
	if !effectiveNet(mf, "m3u8") {
		t.Fatal("m3u8 profile should be online (profile net=true)")
	}
	off := boolPtr(false)
	mf.Net = true
	mf.Profiles["default"] = ProfileDecl{Net: off}
	if effectiveNet(mf, "default") {
		t.Fatal("profile-level net=false must override manifest net=true")
	}
}
