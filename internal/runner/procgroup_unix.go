//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// unixProcessGroup puts a server in its own process group.
//
// Unix already has the mechanism Windows needs a job object for: a signal sent
// to a negative pid goes to every process in the group, and a child inherits
// its parent's group unless it asks otherwise. So the whole tree can be
// reached with one kill.
//
// Putting the server in its own group rather than leaving it in the daemon's
// also stops a Ctrl+C in the terminal the daemon was started from travelling
// to every running world: without it, an operator stopping the daemon by hand
// kills the servers uncleanly instead of letting the daemon stop them.
type unixProcessGroup struct{}

func newProcGroup() procGroup { return unixProcessGroup{} }

func (unixProcessGroup) prepare(cmd *exec.Cmd) {
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Setpgid = true
	cmd.SysProcAttr = attr
}

// attach has nothing to do: Setpgid took effect at fork.
func (unixProcessGroup) attach(*exec.Cmd) error { return nil }

// kill signals the whole group.
func (unixProcessGroup) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("the process is not running")
	}

	// The group id equals the leader's pid, and the leader is the server,
	// because Setpgid with no Pgid makes the child its own leader.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// ESRCH means the group is already gone, which is the outcome asked
		// for. Anything else is worth reporting, but the direct child is still
		// worth trying: a failed Setpgid would leave it outside any group.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if killErr := cmd.Process.Kill(); killErr != nil {
			return fmt.Errorf("killing the process group: %w", err)
		}
	}
	return nil
}

// close has nothing to release: a process group is not a handle.
func (unixProcessGroup) close() error { return nil }
