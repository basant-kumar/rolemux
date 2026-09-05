//go:build windows

package runner

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var childJobs sync.Map

func configureChildProcess(cmd *exec.Cmd) {
	// Keep the process suspended until superviseChildProcess has assigned it
	// to the kill-on-close job. This closes the window in which warp could
	// spawn Codex before the job assignment completes.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
}

// superviseChildProcess assigns the suspended process to a kill-on-close Job
// Object before resuming its primary thread. Warp launches Codex as a
// descendant, so terminating the job covers both processes on Windows.
func superviseChildProcess(cmd *exec.Cmd) (func(), error) {
	if cmd == nil || cmd.Process == nil {
		return func() {}, windows.ERROR_INVALID_HANDLE
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return func() {}, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return func() {}, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return func() {}, err
	}
	err = windows.AssignProcessToJobObject(job, process)
	_ = windows.CloseHandle(process)
	if err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return func() {}, err
	}
	childJobs.Store(cmd, job)
	if err := resumeSuspendedProcess(cmd.Process.Pid); err != nil {
		childJobs.Delete(cmd)
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return func() {}, err
	}
	return func() { releaseChildProcess(cmd) }, nil
}

func resumeSuspendedProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	err = windows.Thread32First(snapshot, &entry)
	for err == nil {
		if entry.OwnerProcessID == uint32(pid) {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				err = openErr
				break
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			return nil
		}
		err = windows.Thread32Next(snapshot, &entry)
	}
	if err == windows.ERROR_NO_MORE_FILES {
		return windows.ERROR_NOT_FOUND
	}
	return err
}

func releaseChildProcess(cmd *exec.Cmd) {
	if value, ok := childJobs.LoadAndDelete(cmd); ok {
		_ = windows.CloseHandle(value.(windows.Handle))
	}
}

func terminateJobOrProcess(cmd *exec.Cmd, exitCode uint32) {
	if value, ok := childJobs.Load(cmd); ok {
		_ = windows.TerminateJobObject(value.(windows.Handle), exitCode)
		return
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func terminateChildProcess(cmd *exec.Cmd) {
	terminateJobOrProcess(cmd, 1)
}

func forceKillChildProcess(cmd *exec.Cmd) {
	terminateJobOrProcess(cmd, 1)
}
