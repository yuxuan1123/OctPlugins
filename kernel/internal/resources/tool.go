package resources

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/octplugin/kernel/internal/process"
)

// kind=tool：定位可执行文件（§14.2 三选一）与启动（§14.3，与定位正交）。

// LocateKind 定位方案（§14.2）。
const (
	LocateBundled      = "bundled"      // 宿主按声明地址安装到 tools/，校验 sha256
	LocateExplicitPath = "explicitPath" // 用户指定绝对路径
	LocateEnv          = "env"          // 默认：按常见环境变量名在 PATH 中查找
)

// LaunchKind 启动方式（§14.3，与定位正交，二选一）。
const (
	LaunchJSONConfig  = "jsonConfig"
	LaunchSelfManaged = "selfManaged"
	LaunchScript      = "script"
)

// NewTool 构造一个工具登记录目（§14.2/§14.3）。
// method 为定位方案（bundled / explicitPath / env，默认 env）；
// declaredPath 为声明的可执行路径（bundled/explicitPath 用）；toolsRoot 为 tools/ 根。
// spawn 钩子执行定位（Locate）并把可得路径写入目录项，供 registry 落盘与 Acquire 复用。
func NewTool(id, method, declaredPath, toolsRoot string) *Entry {
	e := &Entry{
		ID:    id,
		Kind:  KindTool,
		State: "registered",
	}
	e.spawn = func(ctx context.Context) error {
		if method == "" {
			method = LocateEnv
		}
		// 复用已有定位路径（避免每次都是冷定位）。
		e.LocateResult = Locate(method, id, declaredPath, toolsRoot)
		return nil
	}
	e.stop = func() error {
		e.State = "registered"
		return nil
	}
	return e
}

// LocateResult 定位结果。
type LocateResult struct {
	Path  string
	Found bool
	Err   error
}

// Locate 按定位方案定位工具（§14.2 三选一；两个维度各自单选，MUST NOT 展开九分支）。
func Locate(method, id, declaredPath, toolsRoot string) LocateResult {
	switch method {
	case LocateBundled:
		p := filepath.Join(toolsRoot, id)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return LocateResult{Path: p, Found: true}
		}
		return LocateResult{Err: fmt.Errorf("bundled tool %s not found under tools/ (sha256 unverified)", id)}
	case LocateExplicitPath:
		if declaredPath == "" {
			return LocateResult{Err: fmt.Errorf("explicitPath tool %s has no declared path", id)}
		}
		if st, err := os.Stat(declaredPath); err == nil && !st.IsDir() {
			return LocateResult{Path: declaredPath, Found: true}
		}
		return LocateResult{Err: fmt.Errorf("explicitPath tool %s not found at %s", id, declaredPath)}
	default: // env（默认）
		if declaredPath != "" {
			// 声明了常见可执行名（如 ollama）：直接 LookPath
			if p, err := exec.LookPath(declaredPath); err == nil {
				return LocateResult{Path: p, Found: true}
			}
		}
		if p, err := exec.LookPath(id); err == nil {
			return LocateResult{Path: p, Found: true}
		}
		// LookPath 失败后的兜底：常见安装路径 + 系统注册表登记（§14.2 env 扩展）。
		// 覆盖 LibreOffice（soffice 不在 PATH）、便携 ffmpeg 等已安装但未入 PATH 的情况。
		if p, ok := locateCommon(id); ok {
			return LocateResult{Path: p, Found: true}
		}
		return LocateResult{Err: fmt.Errorf("tool %s not found in PATH (E_TOOL_UNAVAILABLE)", id)}
	}
}

// locateCommon 在 LookPath 失败后探测常见安装位置：先按系统注册表登记（仅 Windows），
// 再按跨平台常见路径逐一 stat。返回第一个存在的可执行文件。
func locateCommon(id string) (string, bool) {
	for _, p := range append(registryToolPaths(id), wellKnownPaths(id)...) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// wellKnownPaths 常见安装路径（跨平台硬编码探测；注册表未覆盖时兜底）。
func wellKnownPaths(id string) []string {
	var base []string
	switch id {
	case "soffice", "libreoffice", "soffice.bin", "soffice.exe":
		base = []string{
			`C:\Program Files\LibreOffice\program\soffice.exe`,
			`C:\Program Files (x86)\LibreOffice\program\soffice.exe`,
			`/usr/bin/soffice`, `/usr/local/bin/soffice`, `/opt/libreoffice/program/soffice`,
		}
		if ad := os.Getenv("LOCALAPPDATA"); ad != "" {
			base = append(base, filepath.Join(ad, `Programs\LibreOffice\program\soffice.exe`))
		}
	case "ffmpeg":
		base = []string{
			`C:\Program Files\ffmpeg\bin\ffmpeg.exe`, `/usr/bin/ffmpeg`, `/usr/local/bin/ffmpeg`,
		}
	case "ffprobe":
		base = []string{
			`C:\Program Files\ffmpeg\bin\ffprobe.exe`, `/usr/bin/ffprobe`, `/usr/local/bin/ffprobe`,
		}
	case "pandoc":
		base = []string{`C:\Program Files\Pandoc\pandoc.exe`, `/usr/bin/pandoc`, `/usr/local/bin/pandoc`}
	}
	return base
}

// Spawn 派生工具进程（§14.6 start）：env 由调用方组装；失败返回错误。
func Spawn(ctx context.Context, execPath string, args []string, dir string, env []string) (*process.SpawnResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := process.Spawn(execPath, args, dir, env, nil)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		process.KillTree(res.Cmd)
	}()
	return res, nil
}
