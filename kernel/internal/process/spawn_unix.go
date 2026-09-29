//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

// applyPlatformAttrs Unix：进程组隔离（§13.2）。PR_SET_PDEATHSIG 在中间夹有
// /bin/sh -c 时失效，进程组更可靠。
func applyPlatformAttrs(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

// PreStartAttrs Unix：派生前建立进程组（§13.2），供直接使用 exec.Command 的
// 调用方（如 lifecycle.Plugin.Start）复用——不设 Setpgid 则 KillTree 无法回收整树。
func PreStartAttrs(cmd *exec.Cmd) { _ = applyPlatformAttrs(cmd) }

// attachJob Unix 无 Job Object，返回空释放器。
func attachJob(cmd *exec.Cmd) (func(), error) {
	return func() {}, nil
}
