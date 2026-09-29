// Package process 提供子进程派生与回收的通用包装。
//
// 对齐《理想架构.md》§12.3（派生）/ §13（进程树回收）：
//   - 环境组装（cleanEnv + OCT_* 注入）见 env.go；
//   - 平台属性（Windows Job Object / Unix 进程组）由 *_windows/_unix 文件实现；
//   - 谁派生谁负责回收：所有子进程 MUST 执行 cmd.Wait()，否则产生僵尸进程。
package process

import (
	"io"
	"os"
	"os/exec"
)

// SpawnResult 一次派生的产物：进程 + 协议管道 + 平台资源释放器。
type SpawnResult struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	// Close 释放平台句柄（Job Object 等）；须在进程退出后调用。
	Close func()
}

// Spawn 派生一个子进程（§12.3）：
//   - env 由调用方用 BuildPluginEnv 组装（含 OCT_* 注入）；
//   - stdin/stdout 为协议管道（fd 1/0），stderr 继承宿主 stderr；
//   - logW 非空时挂到 fd 3 作结构化日志流；
//   - 平台属性（Job Object / 进程组）由 applyPlatformAttrs + attachJob 保证。
func Spawn(execPath string, args []string, dir string, env []string, logW *os.File) (*SpawnResult, error) {
	cmd := exec.Command(execPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	if err := applyPlatformAttrs(cmd); err != nil {
		return nil, err // §12.3：平台属性失败 MUST 中止启动，MUST NOT 降级裸跑
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if logW != nil {
		cmd.ExtraFiles = []*os.File{logW} // fd 3 结构化日志
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	closeJob, err := attachJob(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return &SpawnResult{Cmd: cmd, Stdin: stdin, Stdout: stdout, Close: closeJob}, nil
}
