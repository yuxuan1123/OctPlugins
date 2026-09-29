//go:build !windows

package lifecycle

import "os/exec"

// attachLifecycle 非 Windows 平台：进程树回收不依赖额外装配。
//
// §13.2 的进程组隔离（Setpgid）已由 Plugin.Start 在 exec.Command 派生前经
// process.PreStartAttrs 设置；终止整棵树由 Plugin.Kill/Stop 调用 process.KillTree
// （群发 SIGTERM 到进程组）完成。此处返回空释放器，仅保持与 Windows 的接口一致。
func attachLifecycle(cmd *exec.Cmd, memBytes int) (closeJob func(), err error) {
	_ = cmd
	_ = memBytes
	return func() {}, nil
}
