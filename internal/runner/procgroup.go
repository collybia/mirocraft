package runner

import "os/exec"

// procGroup ties a launched server and everything it spawns into one unit, so
// that killing it reaches the whole tree.
//
// This matters more than it looks. A Minecraft server is a JVM, and a JVM
// started by a wrapper script, or a modded server that forks a helper, leaves
// children behind when only the direct child is signalled. Those children keep
// the world files open and the port bound, and the next start fails with
// "address already in use" for reasons the operator cannot see.
//
// The two platforms solve it differently — a job object on Windows, a process
// group on Unix — which is exactly the kind of difference the project's rules
// say to hide behind an interface rather than spread `runtime.GOOS` around.
type procGroup interface {
	// prepare adjusts the command before it starts. Called once, before Start.
	prepare(cmd *exec.Cmd)

	// attach brings the started process into the group. Called immediately
	// after Start; a failure is worth logging but not worth refusing to run a
	// server over, so it returns an error the caller may downgrade.
	attach(cmd *exec.Cmd) error

	// kill terminates the whole group.
	kill(cmd *exec.Cmd) error

	// close releases whatever the group holds, after the process is reaped.
	close() error
}
