//go:build windows

package runner

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsJob is a job object holding one server and its descendants.
//
// Windows has no process groups that survive for signalling, and
// TerminateProcess reaches only the process it names. A job object is the
// mechanism the platform actually provides for "this process and everything it
// starts": children are added automatically, and terminating the job
// terminates all of them.
type windowsJob struct {
	mu     sync.Mutex
	handle windows.Handle
	closed bool
}

func newProcGroup() procGroup { return &windowsJob{} }

// prepare asks for a new process group.
//
// Not for killing — the job object does that — but so a Ctrl+C in the console
// the daemon was started from does not travel to the servers. Without it, an
// operator stopping the daemon by hand takes every world down with an
// unclean kill rather than the graceful stop the daemon would have performed.
func (j *windowsJob) prepare(cmd *exec.Cmd) {
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr = attr
}

// attach creates the job and puts the process in it.
//
// The job is created here rather than in prepare because it is only useful
// once there is a process to hold, and a job created for a launch that failed
// would be a leaked handle.
//
// There is a window between CreateProcess returning and the assignment landing
// in which a child started by the server would escape the job. Closing it
// properly needs the process created suspended and resumed after assignment,
// and os/exec does not hand back the thread handle that would take. The window
// is microseconds and a JVM has not reached its own main by then, so the
// trade is documented rather than papered over.
func (j *windowsJob) attach(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("the process is not running")
	}

	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("creating a job object: %w", err)
	}

	// KILL_ON_JOB_CLOSE is the point of the exercise: when the daemon exits,
	// its handle closes and the servers go with it.
	//
	// That is deliberate rather than incidental. A server whose daemon is gone
	// cannot be stopped, restarted or read from the panel, but still holds its
	// port and its world files — so the next daemon cannot start it either.
	// An operator who wants a server to outlive the daemon is asking for a
	// service, not a panel-managed process.
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("configuring the job object: %w", err)
	}

	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("opening the server process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(process) }()

	if err := windows.AssignProcessToJobObject(handle, process); err != nil {
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("assigning the server to its job object: %w", err)
	}

	j.mu.Lock()
	j.handle = handle
	j.mu.Unlock()
	return nil
}

// kill terminates every process in the job.
func (j *windowsJob) kill(cmd *exec.Cmd) error {
	j.mu.Lock()
	handle, closed := j.handle, j.closed
	j.mu.Unlock()

	// No job means the assignment failed earlier; killing the one process the
	// runner does know about is better than refusing.
	if handle == 0 || closed {
		if cmd.Process == nil {
			return fmt.Errorf("the process is not running")
		}
		return cmd.Process.Kill()
	}

	if err := windows.TerminateJobObject(handle, 1); err != nil {
		return fmt.Errorf("terminating the job object: %w", err)
	}
	return nil
}

func (j *windowsJob) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.handle == 0 || j.closed {
		return nil
	}
	j.closed = true
	return windows.CloseHandle(j.handle)
}
