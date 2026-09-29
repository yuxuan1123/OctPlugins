//go:build !windows

package lifecycle

import "os/exec"

// hideConsoleWindow 非 Windows 平台无控制台窗口概念，空实现（保持跨平台编译一致）。
func hideConsoleWindow(cmd *exec.Cmd) {}
