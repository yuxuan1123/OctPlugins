//go:build !windows

package resources

// registryToolPaths 非 Windows 平台无系统注册表，返回 nil（env 定位仅靠
// LookPath + wellKnownPaths 常见路径探测）。
func registryToolPaths(id string) []string { return nil }
