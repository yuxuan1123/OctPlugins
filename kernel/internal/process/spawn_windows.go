//go:build windows

package process

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

// Windows 进程树回收（§13.1）：无条件建立 Job Object（即使不设内存上限），
// 设置 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE —— 宿主退出/关闭 Job 句柄时整树终止，
// 孙子进程无法逃逸。
//
// 严格遵循「CREATE_SUSPENDED → AssignProcessToJobObject → ResumeThread」：
// applyPlatformAttrs 设 CREATE_SUSPENDED（进程创建即挂起），cmd.Start() 后进程
// 尚未执行任何指令，attachJob 完成 Assign 后再 NtResumeProcess 恢复，
// 消除「先 Start 后 Assign」的孙子进程逃逸竞态（§13.1 MUST NOT）。

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	createSuspended                   = 0x00000004
	procSetQuota                      = 0x0100
	procSuspendResume                 = 0x0800
	procTerminate                     = 0x0001
)

type winBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type winExtendedLimitInformation struct {
	BasicLimitInformation winBasicLimitInformation
	IoInfo                struct{ Iov [6]uint64 }
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// applyPlatformAttrs Windows 侧以 CREATE_SUSPENDED 挂起再派生（§13.1 时序），
// Job 在 Start 后经 attachJob 建立并 Assign，随后 NtResumeProcess 恢复。
func applyPlatformAttrs(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	return nil
}

// PreStartAttrs Windows：CREATE_SUSPENDED 已由宿主的 hideConsoleWindow 设置，
// 此处不干预，避免覆盖其 CreationFlags（§13.1 时序由 Job 建立方保证）。
func PreStartAttrs(cmd *exec.Cmd) {}

// attachJob 无条件为 cmd 建立 Job Object 并挂接 KILL_ON_JOB_CLOSE；返回句柄释放器。
func attachJob(cmd *exec.Cmd) (func(), error) {
	mod := syscall.NewLazyDLL("kernel32.dll")
	fCreate := mod.NewProc("CreateJobObjectW")
	fSet := mod.NewProc("SetInformationJobObject")
	fAssign := mod.NewProc("AssignProcessToJobObject")
	fClose := mod.NewProc("CloseHandle")

	job, _, _ := fCreate.Call(0, 0)
	if job == 0 {
		return func() {}, fmt.Errorf("CreateJobObject failed")
	}
	info := winExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	rc, _, _ := fSet.Call(job, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if rc == 0 {
		_, _, _ = fClose.Call(job)
		return func() {}, fmt.Errorf("SetInformationJobObject failed")
	}
	// 通过 pid 打开进程句柄（os.Process.Handle 未导出）；含 PROCESS_SUSPEND_RESUME 以便恢复。
	ph, err := syscall.OpenProcess(procSetQuota|procSuspendResume|procTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		_, _, _ = fClose.Call(job)
		return func() {}, fmt.Errorf("OpenProcess(pid=%d): %w", cmd.Process.Pid, err)
	}
	// Assign 必须在 Resume 之前（§13.1）——进程经 CREATE_SUSPENDED 创建，此刻未执行任何指令。
	rc, _, _ = fAssign.Call(job, uintptr(ph))
	if rc == 0 {
		_, _, _ = fClose.Call(uintptr(ph))
		_, _, _ = fClose.Call(job)
		return func() {}, fmt.Errorf("AssignProcessToJobObject failed")
	}
	// Assign 成功后再恢复执行；恢复失败同样终止（Spawn 上层会 Kill）。
	fResume := syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
	if r, _, _ := fResume.Call(uintptr(ph)); r != 0 {
		_, _, _ = fClose.Call(uintptr(ph))
		_, _, _ = fClose.Call(job)
		return func() {}, fmt.Errorf("NtResumeProcess failed")
	}
	_, _, _ = fClose.Call(uintptr(ph))
	return func() { _, _, _ = fClose.Call(job) }, nil
}
