package pipkin

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func runFlashProcess(command *exec.Cmd) error {
	return runFlashProcessInJob(command, windows.TerminateJobObject)
}

func runFlashProcessInJob(command *exec.Cmd, terminateJob func(windows.Handle, uint32) error) (result error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("could not create flashing-tool process job: %w", err)
	}
	var closeOnce sync.Once
	var closeErr error
	closeJob := func() error {
		closeOnce.Do(func() { closeErr = windows.CloseHandle(job) })
		return closeErr
	}
	defer func() {
		if err := closeJob(); err != nil {
			result = errors.Join(result, fmt.Errorf("%w: %w", errFlashProcessCleanup, err))
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("could not contain flashing-tool processes: %w", err)
	}
	// A PyInstaller child can acquire the serial port. Start its parent suspended
	// so it cannot create a child before assignment to this private, non-breakaway
	// job. KILL_ON_JOB_CLOSE also covers an explicit termination failure.
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED}
	var mu sync.Mutex
	assigned, cancelled := false, false
	command.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		cancelled = true
		if !assigned {
			return command.Process.Kill()
		}
		if err := terminateJob(job, 1); err != nil {
			if cleanupErr := closeJob(); cleanupErr != nil {
				parentErr := command.Process.Kill()
				return fmt.Errorf("%w: %w", errFlashProcessCleanup, errors.Join(err, cleanupErr, parentErr))
			}
		}
		return nil
	}
	if err := command.Start(); err != nil {
		return err
	}
	mu.Lock()
	if cancelled {
		mu.Unlock()
		return command.Wait()
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		windows.CloseHandle(process)
		if err == nil {
			assigned = true
			err = resumeFlashThread(uint32(command.Process.Pid))
		}
	}
	mu.Unlock()
	if err != nil {
		// The parent has not run its initial thread if containment/resume fails.
		// Kill it before waiting, and close the job on return in every case.
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("could not start contained flashing tool: %w", err)
	}
	return command.Wait()
}

func resumeFlashThread(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, resumeErr := windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		return resumeErr
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	return errors.New("could not locate the flashing tool's suspended initial thread")
}
