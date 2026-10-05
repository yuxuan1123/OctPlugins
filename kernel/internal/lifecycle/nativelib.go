package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// 本文件：原生共享库校验（转化域 §20.4）。
//
// 缺口：`lockHash` 锁的是代码依赖清单（requirements.txt / package.json / start.lock），
// **锁不住 `.dll` / `.so` / `.dylib`**。而 purego / cgo 类 tool（如 RapidOCR dlopen
// onnxruntime）的正确性完全依赖这些库的版本与内容——§20.2 更明确要求
// 「ORT 共享库版本必须与 C API 头文件严格对齐，不匹配直接加载失败」。
//
// 故 manifest 的 nativeLibs 声明与 spawn 前校验一起纳入 §20：
//   - SHA256 非空 → 校验，不匹配拒绝启动（E_DEPS_HASH_MISMATCH）；
//   - SHA256 为空 → 登记留痕（系统提供的库跨机器哈希不固定），不阻断启动。

// resolveNativeLibPath 解析 nativeLibs.path：支持 `env:NAME` 与绝对/相对路径。
func resolveNativeLibPath(declared string) string {
	p := strings.TrimSpace(declared)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "env:") {
		name := strings.TrimSpace(strings.TrimPrefix(p, "env:"))
		if name == "" {
			return ""
		}
		return strings.TrimSpace(os.Getenv(name))
	}
	return p
}

// verifyNativeLibs 在 spawn 前校验某插件声明的原生库清单。
//
// 返回错误即拒绝启动（§20 MUST：校验失败不得继续派生进程）。
// 未声明 nativeLibs 的插件直接通过（向后兼容，等价旧行为）。
//
// Optional 的语义严格限定为「容忍缺失」：Optional=true 且库不存在时只记日志，
// 让 tool 在运行时按 E_TOOL_UNAVAILABLE 自行降级；但**哈希不匹配一律拒绝**，
// 因为「库在、内容不对」正是 §20.2 要防的崩溃场景。
func verifyNativeLibs(mf Manifest) error {
	if len(mf.NativeLibs) == 0 {
		return nil
	}
	for _, lib := range mf.NativeLibs {
		path := resolveNativeLibPath(lib.Path)
		if path == "" {
			if lib.Optional {
				log.Printf("[%s] nativeLibs: %q 无法解析（env 未设置），optional → 跳过校验",
					mf.ID, lib.Path)
				continue
			}
			return fmt.Errorf("nativeLibs: 插件 %s 声明的库路径 %q 无法解析（env: 变量未设置？）",
				mf.ID, lib.Path)
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			if lib.Optional {
				log.Printf("[%s] nativeLibs: %s 不存在，optional → 跳过校验（运行时由 tool 降级）",
					mf.ID, path)
				continue
			}
			return fmt.Errorf("nativeLibs: 插件 %s 的原生库不存在：%s", mf.ID, path)
		}
		want := strings.ToLower(strings.TrimSpace(lib.SHA256))
		if want == "" {
			log.Printf("[%s] nativeLibs: %s 已登记（未声明 sha256，跳过校验）%s",
				mf.ID, filepath.Base(path), noteSuffix(lib.Note))
			continue
		}
		got, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("nativeLibs: 读取 %s 失败：%w", path, err)
		}
		if got != want {
			return fmt.Errorf("nativeLibs: %s sha256 不匹配（期望 %s，实际 %s）——"+
				"ORT 共享库版本须与工具绑定时的 C API 头文件严格对齐（§20.2），拒绝启动",
				path, want, got)
		}
		log.Printf("[%s] nativeLibs: %s sha256 校验通过", mf.ID, filepath.Base(path))
	}
	return nil
}

func noteSuffix(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return "（" + note + "）"
}

// fileSHA256 计算文件 sha256（小写 hex）。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
