//go:build !windows

package runner

import (
	"os"
	"strconv"
)

// containerUser makes the container run as the daemon's own user.
//
// Without it the server runs as root and every file it writes into the mounted
// directory is root-owned — so the panel's own file manager, running as the
// daemon's user, cannot edit a config or delete a world it just created. The
// operator sees a file browser that lists everything and can change nothing,
// and the cause is invisible from the panel.
func containerUser() string {
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}
