package api

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// diskUsageTTL is how long a measured usage figure is reused.
//
// Measuring means walking every file of every server the account owns, which
// on a world with a hundred thousand region files is not something to do on
// each of a hundred uploads. Half a minute is short enough that the figure
// still describes the disk and long enough that a burst of writes measures
// once. The window is the deliberate slack in the limit: an account at its
// quota can overshoot by whatever it manages to write in it, and a limit that
// is thirty seconds late is worth far more than one that is never applied.
const diskUsageTTL = 30 * time.Second

// diskUsage caches how much disk each account occupies.
type diskUsage struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]diskUsageEntry
}

type diskUsageEntry struct {
	bytes      int64
	measuredAt time.Time
}

func newDiskUsage() *diskUsage {
	return &diskUsage{now: time.Now, entries: make(map[string]diskUsageEntry)}
}

// get returns the cached figure for a user, if it is still fresh.
func (d *diskUsage) get(userID string) (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entry, ok := d.entries[userID]
	if !ok || d.now().Sub(entry.measuredAt) > diskUsageTTL {
		return 0, false
	}
	return entry.bytes, true
}

func (d *diskUsage) set(userID string, bytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[userID] = diskUsageEntry{bytes: bytes, measuredAt: d.now()}
}

// add records data written since the last measurement, so a run of uploads
// inside one TTL window still counts towards the limit.
func (d *diskUsage) add(userID string, bytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entry, ok := d.entries[userID]
	if !ok {
		return
	}
	entry.bytes += bytes
	d.entries[userID] = entry
}

// forget drops the cached figure, for an operation whose size is not known
// until after it has run.
func (d *diskUsage) forget(userID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, userID)
}

// diskUsedBy returns how much disk an account's servers occupy.
func (a *API) diskUsedBy(ctx context.Context, userID string) (int64, error) {
	if used, ok := a.diskUsage.get(userID); ok {
		return used, nil
	}

	servers, err := a.store.Servers.List(ctx, store.ServerFilter{OwnerID: userID})
	if err != nil {
		return 0, err
	}

	var total int64
	for _, server := range servers {
		size, err := directorySize(a.serverDir(server))
		if err != nil {
			return 0, err
		}
		total += size

		// Backups count too. They live outside the server directory, and an
		// account that could not fill the disk with worlds could otherwise
		// fill it with copies of them. Their sizes are recorded when they
		// finish, so this costs a query rather than another walk.
		records, err := a.store.Backups.ListByServer(ctx, server.ID)
		if err != nil {
			return 0, err
		}
		for _, record := range records {
			total += record.SizeBytes
		}
	}

	a.diskUsage.set(userID, total)
	return total, nil
}

// directorySize adds up the files under a directory.
//
// A directory that is not there yet counts as nothing rather than as an
// error: a server that has never started has no files, and that is a size.
func directorySize(dir string) (int64, error) {
	if dir == "" {
		return 0, nil
	}

	var total int64
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A file that vanished mid-walk is a running server rotating its
			// logs, not a failure to measure.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

// enforceDiskQuota refuses a write that would take an account past its disk
// allowance, where wants is what the request is about to add.
//
// The allowance is per account rather than per server, because that is what an
// operator is actually sharing out: one person with ten servers has ten
// chances to fill the disk.
//
// Nothing to enforce is the common case — a single-user panel leaves the
// allowance at zero, which is unlimited — and that case costs one row lookup
// and no directory walk at all.
func (a *API) enforceDiskQuota(w http.ResponseWriter, r *http.Request, ownerID string, wants int64) bool {
	user, err := a.store.Users.GetByID(r.Context(), ownerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read the account")
		return false
	}
	if user.MaxDiskMb <= store.Unlimited {
		return true
	}

	used, err := a.diskUsedBy(r.Context(), ownerID)
	if err != nil {
		// Refusing here would make an unreadable directory into an outage.
		// The limit is a fair-share device, not a security boundary: the
		// file sandbox is what keeps a user inside their own directory.
		a.log.Warn("measuring disk usage failed",
			slog.String("user_id", ownerID), slog.String("error", err.Error()))
		return true
	}

	limit := int64(user.MaxDiskMb) << 20
	if used+wants > limit {
		writeErrorDetails(w, http.StatusUnprocessableEntity, "insufficient_resources",
			"this would exceed your disk allowance",
			map[string]any{
				"limit_mb":     user.MaxDiskMb,
				"used_mb":      used >> 20,
				"requested_mb": wants >> 20,
			})
		return false
	}
	return true
}

// recordDiskWritten adds what an operation wrote to the cached figure.
func (a *API) recordDiskWritten(ownerID string, bytes int64) {
	if bytes > 0 {
		a.diskUsage.add(ownerID, bytes)
	}
}

// contentLength is what a request says it will send, or zero when it does not
// say. Used as the size of a write before it happens; the sandbox's own limit
// is what stops a body that lied.
func contentLength(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}

// fileSize reports the size of a file, or zero if it cannot be read.
func fileSize(path string) int64 {
	// A path the caller resolved through the sandbox.
	info, err := os.Stat(path) // #nosec G304,G703 -- resolved by the caller
	if err != nil {
		return 0
	}
	return info.Size()
}
