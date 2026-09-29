//go:build windows

package lifecycle

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

// Windows 进程树回收（§13.1）与可选内存上限（§12.3 / §13.1 MUST 时序）。
//
// 严格遵循「CREATE_SUSPENDED → AssignProcessToJobObject → ResumeThread」：
//   - hideConsoleWindow 在 Start 前设 CREATE_SUSPENDED，进程创建后立即挂起；
//   - attachLifecycle 无条件建立 Job（KILL_ON_JOB_CLOSE，即使 memBytes=0），
//     将进程 Assign 进 Job 后再 NtResumeProcess 恢复，
//     — 彻底消除「先 Start 后 Assign」的孙子进程逃逸竞态（§13.1 MUST NOT）；
//     — Job 不是内存上限的前提，即使不设内存上限也必须建 Job（§13.1）。
//   - attachLifecycle 失败 MUST 中止启动（§12.3），MUST NOT 降级为裸跑。

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitProcessMemory       = 0x00000100
	jobObjectLimitKillOnJobClose      = 0x00002000
)

// createSuspended 由 hideConsoleWindow（stdproc_windows.go）使用：进程创建即挂起。
const createSuspended = 0x00000004

const (
	procSetQuota      = 0x0100
	procSuspendResume = 0x0800
	procTerminate     = 0x0001
)

type winIOCOUNTERS struct{ Iov [6]uint64 }

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
	IoInfo                winIOCOUNTERS
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// attachLifecycle 无条件为插件建立 Job（KILL_ON_JOB_CLOSE），可选设内存上限，
// 并按 §13.1 时序 Assign 后恢复进程。返回的 closeJob 在进程停止后调用释放句柄。
// 任何一步失败：强制终止进程并返回错误（§12.3 MUST 中止启动）。
func attachLifecycle(cmd *exec.Cmd, memBytes int) (closeJob func(), err error) {
	mod := syscall.NewLazyDLL("kernel32.dll")
	nt := syscall.NewLazyDLL("ntdll.dll")
	fCreate := mod.NewProc("CreateJobObjectW")
	fSet := mod.NewProc("SetInformationJobObject")
	fAssign := mod.NewProc("AssignProcessToJobObject")
	fClose := mod.NewProc("CloseHandle")
	fResume := nt.NewProc("NtResumeProcess")

	// fail 统一失败路径：终止（可能仍挂起的）进程 + 释放已建 Job。
	fail := func(e error) (func(), error) {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return func() {}, e
	}

	job, _, _ := fCreate.Call(0, 0)
	if job == 0 {
		return fail(fmt.Errorf("CreateJobObject failed"))
	}
	info := winExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if memBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= jobObjectLimitProcessMemory
		info.ProcessMemoryLimit = uintptr(memBytes)
	}
	rc, _, _ := fSet.Call(job, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if rc == 0 {
		_, _, _ = fClose.Call(job)
		return fail(fmt.Errorf("SetInformationJobObject failed"))
	}

	ph, err := syscall.OpenProcess(procSetQuota|procSuspendResume|procTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		_, _, _ = fClose.Call(job)
		return fail(fmt.Errorf("OpenProcess(pid=%d): %w", cmd.Process.Pid, err))
	}
	// Assign 必须在 Resume 之前完成（§13.1 时序）——创建时已被 CREATE_SUSPENDED 挂起。
	rc, _, _ = fAssign.Call(job, uintptr(ph))
	if rc == 0 {
		_, _, _ = fClose.Call(uintptr(ph))
		_, _, _ = fClose.Call(job)
		return fail(fmt.Errorf("AssignProcessToJobObject failed"))
	}
	// Assign 成功后再恢复执行；恢复失败同样终止。
	rc, _, _ = fResume.Call(uintptr(ph))
	_, _, _ = fClose.Call(uintptr(ph))
	if rc != 0 {
		_, _, _ = fClose.Call(job)
		return fail(fmt.Errorf("NtResumeProcess failed"))
	}
	return func() { _, _, _ = fClose.Call(job) }, nil
}
