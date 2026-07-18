//go:build windows

package tool

import (
	"context"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runCommandTree(ctx context.Context, command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if err := command.Start(); err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err == nil {
		defer windows.CloseHandle(job)
		limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	}
	if err == nil {
		process, openErr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
		if openErr == nil {
			err = windows.AssignProcessToJobObject(job, process)
			windows.CloseHandle(process)
		} else {
			err = openErr
		}
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case waitErr := <-done:
		return waitErr
	case <-ctx.Done():
		if err == nil {
			_ = windows.TerminateJobObject(job, 1)
		} else {
			_ = command.Process.Kill()
		}
		<-done
		return ctx.Err()
	}
}
