//go:build windows

package claudecode

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

// track puts the program in a job object that ends all of it when closed,
// including if Hostbay itself dies.
func track(cmd *exec.Cmd) func() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return func() { _ = cmd.Process.Kill() }
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err == nil {
		var h windows.Handle
		h, err = windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if err == nil {
			err = windows.AssignProcessToJobObject(job, h)
			windows.CloseHandle(h)
		}
	}
	if err != nil {
		windows.CloseHandle(job)
		return func() { _ = cmd.Process.Kill() }
	}
	var once sync.Once
	return func() { once.Do(func() { _ = windows.TerminateJobObject(job, 1); windows.CloseHandle(job) }) }
}
