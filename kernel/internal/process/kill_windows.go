//go:build windows

package process

import "os/exec"

// KillTree 终止进程树（§13.1）。Windows 上主进程挂接的 Job Object 带
// KILL_ON_JOB_CLOSE，Kill 主进程即可让整树随 Job 关闭被回收。
func KillTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
