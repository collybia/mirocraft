//go:build windows

package main

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceStopTimeout is how long the service manager is told to wait.
//
// Generous because stopping means asking every running Minecraft server to
// save and exit, and a world that has not finished saving when the process is
// killed is the one thing an operator cannot get back.
const serviceStopTimeout = 60 * time.Second

// runUnderServiceManager runs the daemon under the Windows service control
// manager, or directly when there is no manager to answer to.
//
// A plain executable cannot be a Windows service: the manager starts it and
// waits to be told the service is running, and a program that never says so is
// killed with "the service did not respond to the start or control request in
// a timely fashion". So the daemon has to speak that protocol itself — which
// also keeps the single-binary rule, where the usual answer is to ship a
// wrapper like NSSM alongside.
func runUnderServiceManager(daemon func(context.Context) error) error {
	inService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("determining whether this is a service: %w", err)
	}
	if !inService {
		return daemon(context.Background())
	}

	handler := &windowsService{daemon: daemon}
	if err := svc.Run("Mirocraft", handler); err != nil {
		return fmt.Errorf("running as a service: %w", err)
	}
	return handler.err
}

// windowsService adapts the daemon to the service control manager.
type windowsService struct {
	daemon func(context.Context) error
	err    error
}

// Execute is the service loop.
func (s *windowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	// Accepted commands are declared up front: a service that does not accept
	// Stop cannot be stopped except by killing it, which for this daemon means
	// killing every world with it.
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.err = s.daemon(ctx)
	}()

	// Reported running immediately rather than after the daemon settles: the
	// manager's start timeout is short, and provisioning a server on first
	// start can take minutes. A daemon that fails afterwards reports it by
	// exiting, which is handled below.
	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case <-done:
			// The daemon stopped on its own — a fatal error at startup, most
			// likely. Exit code 1 so the manager's restart policy applies.
			status <- svc.Status{State: svc.StopPending}
			if s.err != nil {
				return false, 1
			}
			return false, 0

		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus

			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: uint32(serviceStopTimeout.Milliseconds())}
				cancel()

				select {
				case <-done:
				case <-time.After(serviceStopTimeout):
					// Reported rather than hidden: the worlds that had not
					// finished saving are about to be lost, and the operator
					// should be able to find out why.
					return false, 2
				}
				return false, 0
			}
		}
	}
}
