package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件：转化域 §20.4（原生共享库清单校验）的行为锁定。
//
// 注意：NativeLibs 定义在嵌入的 ManifestFields 上，Go 不允许在复合字面量里
// 直接写提升字段，故统一经 nativeLibManifest 构造。

func nativeLibManifest(id string, libs ...NativeLibDecl) Manifest {
	return Manifest{ID: id, ManifestFields: ManifestFields{NativeLibs: libs}}
}

func writeTempLib(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakelib.dll")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sha256Of(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// 未声明 nativeLibs 的插件不受影响（向后兼容）。
func TestNativeLibsAbsentPasses(t *testing.T) {
	if err := verifyNativeLibs(Manifest{ID: "demo"}); err != nil {
		t.Fatalf("no nativeLibs should pass: %v", err)
	}
}

// sha256 匹配 → 通过。
func TestNativeLibsHashMatch(t *testing.T) {
	p := writeTempLib(t, "ort-binary")
	mf := nativeLibManifest("rapidocr-onnx", NativeLibDecl{Path: p, SHA256: sha256Of("ort-binary")})
	if err := verifyNativeLibs(mf); err != nil {
		t.Fatalf("matching sha256 should pass: %v", err)
	}
}

// sha256 大小写不敏感。
func TestNativeLibsHashCaseInsensitive(t *testing.T) {
	p := writeTempLib(t, "ort-binary")
	mf := nativeLibManifest("rapidocr-onnx",
		NativeLibDecl{Path: p, SHA256: strings.ToUpper(sha256Of("ort-binary"))})
	if err := verifyNativeLibs(mf); err != nil {
		t.Fatalf("uppercase sha256 should pass: %v", err)
	}
}

// §20.4：哈希不匹配 MUST 拒绝（否则 ORT 版本与 C API 头失配会直接崩在 dlopen/Dlsym）。
func TestNativeLibsHashMismatchRejected(t *testing.T) {
	p := writeTempLib(t, "ort-binary")
	mf := nativeLibManifest("rapidocr-onnx",
		NativeLibDecl{Path: p, SHA256: sha256Of("different-binary")})
	err := verifyNativeLibs(mf)
	if err == nil {
		t.Fatal("mismatched sha256 must be rejected")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("error should mention sha256: %v", err)
	}
}

// 库文件缺失 → 拒绝启动。
func TestNativeLibsMissingFileRejected(t *testing.T) {
	mf := nativeLibManifest("rapidocr-onnx",
		NativeLibDecl{Path: filepath.Join(t.TempDir(), "nope.dll")})
	if err := verifyNativeLibs(mf); err == nil {
		t.Fatal("missing native lib must be rejected")
	}
}

// sha256 为空 → 只登记不校验（系统提供的库跨机器哈希不固定）。
func TestNativeLibsEmptyHashRecordsOnly(t *testing.T) {
	p := writeTempLib(t, "system-provided")
	mf := nativeLibManifest("rapidocr-onnx", NativeLibDecl{Path: p})
	if err := verifyNativeLibs(mf); err != nil {
		t.Fatalf("empty sha256 should be record-only: %v", err)
	}
}

// env:VARNAME 形式：变量未设置且非 optional → 拒绝（避免静默跳过校验）。
func TestNativeLibsEnvUnsetRejected(t *testing.T) {
	t.Setenv("OCT_TEST_ORT_UNSET", "")
	mf := nativeLibManifest("rapidocr-onnx", NativeLibDecl{Path: "env:OCT_TEST_ORT_UNSET"})
	if err := verifyNativeLibs(mf); err == nil {
		t.Fatal("unresolvable env: path must be rejected when not optional")
	}
}

// optional=true 只容忍「库不存在」：缺失 → 跳过（运行时由 tool 降级）。
func TestNativeLibsOptionalMissingTolerated(t *testing.T) {
	t.Setenv("OCT_TEST_ORT_OPT", "")
	mf := nativeLibManifest("rapidocr-onnx",
		NativeLibDecl{Path: "env:OCT_TEST_ORT_OPT", Optional: true},
		NativeLibDecl{Path: filepath.Join(t.TempDir(), "absent.dll"), Optional: true})
	if err := verifyNativeLibs(mf); err != nil {
		t.Fatalf("optional missing lib should be tolerated: %v", err)
	}
}

// 但 optional **不**容忍哈希不匹配：库在、内容不对是最危险的情形（§20.2）。
func TestNativeLibsOptionalStillRejectsHashMismatch(t *testing.T) {
	p := writeTempLib(t, "wrong-ort")
	mf := nativeLibManifest("rapidocr-onnx",
		NativeLibDecl{Path: p, SHA256: sha256Of("expected-ort"), Optional: true})
	if err := verifyNativeLibs(mf); err == nil {
		t.Fatal("optional must NOT tolerate a sha256 mismatch")
	}
}

// env:VARNAME 形式：变量指向真实文件且哈希匹配 → 通过。
func TestNativeLibsEnvResolved(t *testing.T) {
	p := writeTempLib(t, "env-lib")
	t.Setenv("OCT_TEST_ORT_SET", p)
	mf := nativeLibManifest("rapidocr-onnx",
		NativeLibDecl{Path: "env:OCT_TEST_ORT_SET", SHA256: sha256Of("env-lib")})
	if err := verifyNativeLibs(mf); err != nil {
		t.Fatalf("env-resolved lib should verify: %v", err)
	}
}
