//go:build !windows

package main

import "context"

// runUnderServiceManager just runs the daemon.
//
// systemd needs nothing of the sort: it starts the process, watches it and
// signals it, which is what the daemon already handles.
func runUnderServiceManager(daemon func(context.Context) error) error {
	return daemon(context.Background())
}
