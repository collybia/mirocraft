// Package runner owns the lifecycle of Minecraft server processes and the
// console plumbing around them: capturing output, buffering scrollback,
// fanning lines out to subscribers and writing commands to stdin.
//
// Everything OS-specific lives behind the Runner interface, per the project
// rule that no `runtime.GOOS` checks leak into the rest of the codebase.
package runner

import (
	"context"
	"errors"
	"time"
)

// Status is the lifecycle state of a server, matching the values documented in
// docs/API.md.
type Status string

// IsActive reports whether the status means a process is alive: starting,
// running or on its way down.
func (s Status) IsActive() bool {
	return s == StatusStarting || s == StatusRunning || s == StatusStopping
}

const (
	StatusCreating Status = "creating"
	StatusStopped  Status = "stopped"
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
	StatusCrashed  Status = "crashed"
)

// Lifecycle errors. The API layer maps these onto the documented error codes.
var (
	ErrServerNotFound    = errors.New("server not found")
	ErrAlreadyRunning    = errors.New("server is already running")
	ErrNotRunning        = errors.New("server is not running")
	ErrRunnerUnavailable = errors.New("runner is unavailable")
)

// Server is the subset of server configuration a Runner needs to start a
// process. The full record lives in the store (task 1.1); this stays minimal
// on purpose so the runner does not depend on the persistence layer.
type Server struct {
	ID       string
	Name     string
	Dir      string // working directory, the server's root
	Core     string // paper, vanilla, fabric, ...
	Version  string // Minecraft version
	RAMMb    int
	JavaBin  string   // absolute path to the java binary; "java" falls back to PATH
	JarName  string   // server jar relative to Dir
	JavaArgs []string // extra JVM flags, e.g. Aikar's

	// Port is the port the server listens on. ProcessRunner does not need it —
	// the server reads it from server.properties and binds the host directly —
	// but a container has its own network stack, so DockerRunner has to
	// publish it.
	Port int
	// JavaMajor is the runtime feature version the server needs. Likewise
	// unused by ProcessRunner, which is handed a path to a runtime that has
	// already been chosen; DockerRunner picks an image from it.
	JavaMajor int
}

// Runner starts and supervises server processes.
//
// Note: this replaces the Console(ctx, id) (io.ReadCloser, error) method
// sketched in docs/ARCHITECTURE.md. A single ReadCloser cannot serve several
// console viewers at once, which the WebSocket console requires, so console
// access is split into History (scrollback) and Subscribe (live fan-out).
type Runner interface {
	Start(ctx context.Context, srv *Server) error
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Kill(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (Status, error)

	// Stats reports resource usage for a running server. Sampling failures are
	// not errors: a server can stop between the lookup and the sample, and a
	// half-filled Stats beats blanking the whole page.
	Stats(ctx context.Context, id string) (Stats, error)

	// History returns up to lines most recent console lines, oldest first.
	History(ctx context.Context, id string, lines int) ([]ConsoleLine, error)

	// Subscribe streams new console lines. The returned function unsubscribes
	// and is safe to call more than once.
	Subscribe(ctx context.Context, id string) (<-chan ConsoleLine, func(), error)

	// SubscribeWithHistory atomically returns scrollback plus a live stream, so
	// a console viewer cannot miss a line published between the two. This is
	// what the WebSocket console uses.
	SubscribeWithHistory(ctx context.Context, id string, lines int) ([]ConsoleLine, <-chan ConsoleLine, func(), error)

	// SubscribeStatus streams status changes with the same semantics.
	SubscribeStatus(ctx context.Context, id string) (<-chan Status, func(), error)

	// SendCommand writes a command to the server's stdin. The command must
	// pass ValidateCommand.
	SendCommand(ctx context.Context, id string, cmd string) error

	// Shutdown stops supervising every server and releases all subscriptions.
	// Running processes are left to the caller to stop first.
	Shutdown(ctx context.Context) error
}
