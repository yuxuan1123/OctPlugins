package resources

import (
	"os/exec"
	"sort"
)

// 探测本机已有运行时与后端（§14.2 env 定位的补充 / UI「运行环境」只读视图）。

// RuntimeProbe 一次探测结果。
type RuntimeProbe struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// Discover 探测常见运行时与工具是否可用（PATH 查找）。
func Discover() []RuntimeProbe {
	var out []RuntimeProbe
	for _, name := range []string{
		"python", "python3", "python3.12",
		"node", "npm", "pnpm", "uv", "go", "rustc", "cargo",
		"ffmpeg", "ffprobe", "ollama",
	} {
		if p, err := exec.LookPath(name); err == nil {
			out = append(out, RuntimeProbe{Name: name, Path: p})
		} else {
			out = append(out, RuntimeProbe{Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
