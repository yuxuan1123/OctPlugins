package runtime

import (
	"context"

	"github.com/octplugin/kernel/internal/lifecycle"
)

// Provider §6.5 RuntimeProvider：按语言实现插件代码依赖生命周期。
//
// 每个 provider 负责一种语言的依赖管理——锁生成（GenerateLock）、
// 就绪判定（Ready，§6.4 gating）与后台安装（Install）——并把共享的
// 下载源/缓存配置（IndexURLs/CacheDir/uvBin）交由外层 Installer 持有。
// 结构对齐《理想架构.md》§6.5 提供 per-language Provider 而非单一分支。
type Provider interface {
	// Lang 返回语言标识（"python"/"node"/...），用于错误信息与日志。
	Lang() string
	// IsSupported 判断该 provider 是否接管此插件类型。
	IsSupported(mf lifecycle.Manifest) bool
	// GenerateLock 为插件在 pluginDir 生成锁文件（§6.4）。无锁可生成时返回 nil。
	GenerateLock(ctx context.Context, pluginDir, lockPath string) error
	// Ready 判定插件依赖是否已就绪（§6.4 gating 依据，false 触发后台安装）。
	Ready(mf lifecycle.Manifest) bool
	// Install 在后台安装插件依赖（§6.4）。progress 用于向 UI 广播安装进度阶段（§6.4 MUST）。
	// MUST 可幂等重入（多轮 gating 不会重复安装或破坏已有环境）。
	Install(ctx context.Context, mf lifecycle.Manifest, progress func(phase string)) error
}

// providerFor 按插件类型解析其 Provider（Interp 之外，依赖隔离的按语言分发点）。
func (i *Installer) providerFor(mf lifecycle.Manifest) Provider {
	if isNodeTypeType(mf.Type) {
		return nodeProvider{inst: i}
	}
	if isPythonType(mf.Type) {
		return pythonProvider{inst: i}
	}
	// Go/Rust/未知：二进制产物或未声明的语言，MVP 不做在线安装（§6.5 表）。
	return noopProvider{inst: i}
}

// noopProvider 处理 Go/Rust 等二进制产物语言（§6.5：天然隔离，MUST NOT 建 venv）。
type noopProvider struct{ inst *Installer }

func (noopProvider) Lang() string                                     { return "none" }
func (noopProvider) IsSupported(lifecycle.Manifest) bool              { return false }
func (noopProvider) GenerateLock(context.Context, string, string) error { return nil }
func (noopProvider) Ready(lifecycle.Manifest) bool                    { return true }
func (noopProvider) Install(context.Context, lifecycle.Manifest, func(string)) error {
	return nil
}