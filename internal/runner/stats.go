package runner

import (
	"context"
	"fmt"
	"time"

	// Aliased: the package name collides with this package's own process type.
	gopsprocess "github.com/shirou/gopsutil/v4/process"
)

// Stats is what the runner can tell about a live server process. Everything
// that requires talking to the game itself — players, TPS — is gathered by the
// API through mcping instead, since the runner has no protocol knowledge.
type Stats struct {
	PID        int
	StartedAt  time.Time
	Uptime     time.Duration
	RAMBytes   uint64
	CPUPercent float64
	// RAMLimitBytes is the ceiling the runtime enforces, zero when there is
	// none. It is not the heap size: a container gets the heap plus headroom
	// for everything the JVM allocates outside it, so comparing usage against
	// the heap made a healthy server read as over its limit.
	RAMLimitBytes uint64
}

// Stats reports resource usage for a running server.
//
// Sampling failures are not errors: a process can exit between the lookup and
// the sample, and a half-filled Stats with a correct uptime is more useful to
// the caller than a hard failure.
func (r *ProcessRunner) Stats(ctx context.Context, id string) (Stats, error) {
	p, err := r.lookup(id)
	if err != nil {
		return Stats{}, err
	}

	p.mu.Lock()
	startedAt := p.startedAt
	p.mu.Unlock()

	stats := Stats{StartedAt: startedAt}
	if !startedAt.IsZero() {
		stats.Uptime = time.Since(startedAt)
	}

	if p.cmd == nil || p.cmd.Process == nil {
		return stats, nil
	}
	stats.PID = p.cmd.Process.Pid

	if !p.currentStatus().IsActive() {
		return stats, nil
	}

	proc, err := gopsprocess.NewProcessWithContext(ctx, int32(stats.PID))
	if err != nil {
		// The process can exit between the status check above and this lookup.
		// That leaves the panel showing a server without numbers, which is
		// what it should show, rather than an error.
		return stats, nil //nolint:nilerr // missing statistics are not a failure
	}
	if mem, err := proc.MemoryInfoWithContext(ctx); err == nil && mem != nil {
		stats.RAMBytes = mem.RSS
	}
	if cpu, err := proc.CPUPercentWithContext(ctx); err == nil {
		stats.CPUPercent = cpu
	}

	return stats, nil
}

// PID returns the operating system process id of a running server.
func (r *ProcessRunner) PID(id string) (int, error) {
	p, err := r.lookup(id)
	if err != nil {
		return 0, err
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return 0, fmt.Errorf("server %s has no process: %w", id, ErrNotRunning)
	}
	return p.cmd.Process.Pid, nil
}
