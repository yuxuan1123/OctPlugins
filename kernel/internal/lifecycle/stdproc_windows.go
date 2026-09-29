//go:build windows

package lifecycle

import (
	"os/exec"
	"syscall"
)

// hideConsoleWindow 隐藏子进程的控制台窗口，并以 CREATE_SUSPENDED 挂起再派生
// （§13.1 时序：进程创建即挂起，待 attachLifecycle 将其 Assign 进 Job 后再恢复）。
func hideConsoleWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createSuspended}
}
