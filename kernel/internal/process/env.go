package process

import (
	"os"
	"path/filepath"
	"strings"
)

// 污染型环境变量（《理想架构.md》§11.2.4）：宿主派生插件时 MUST 清除，
// 防止开发机残留变量掩盖编码/输出问题或干扰插件行为。
var pollutedVars = []string{
	"PYTHONVERBOSE", // 每个 import 打印诊断到 stdout，污染协议流
	"PYTHONSTARTUP", // 启动时执行脚本，可向 stdout 打印
	"NODE_OPTIONS",  // 注入 Node 行为，可改 stdout
	"PYTHONUTF8",    // §11.2.1：编码正确性 MUST 由 SDK 自身保证，不得依赖外部变量
	"PYTHONIOENCODING",
}

// CleanEnv 从环境变量切片中剔除污染型变量。
func CleanEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		drop := false
		for _, p := range pollutedVars {
			if strings.HasPrefix(kv, p+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// BuildPluginEnv 组装插件子进程环境：
//
//	CleanEnv(base) + OCT_PLUGIN_HOME / OCT_PLUGIN_DATA / OCT_CHANNEL_TOKEN / PYTHONUNBUFFERED=1。
//
// token / home / data 为空串时跳过对应注入（便于单测与未设 storeDir 的场景）。
// sdkPythonPath 为 sdk/python 根（§16.5 使 oct_sdk 可导入），非空时经 PYTHONPATH 注入。
func BuildPluginEnv(base []string, token, home, data, sdkPythonPath string) []string {
	env := CleanEnv(base)
	env = append(env, "PYTHONUNBUFFERED=1")
	if token != "" {
		env = append(env, "OCT_CHANNEL_TOKEN="+token)
	}
	if home != "" {
		env = append(env, "OCT_PLUGIN_HOME="+home)
	}
	if data != "" {
		env = append(env, "OCT_PLUGIN_DATA="+data)
	}
	if sdkPythonPath != "" {
		// SDK 必须优先于 venv 的 site-packages，避免同名包被插件自身依赖遮蔽。
		// 保留既有 PYTHONPATH（若有），仅在其前注入并去重同区段。
		sdkPythonPath = filepath.Clean(sdkPythonPath)
		prev := os.Getenv("PYTHONPATH")
		joined := sdkPythonPath
		for _, seg := range filepath.SplitList(prev) {
			if seg != "" && seg != sdkPythonPath {
				joined += string(os.PathListSeparator) + seg
			}
		}
		env = append(env, "PYTHONPATH="+joined)
	}
	return env
}
