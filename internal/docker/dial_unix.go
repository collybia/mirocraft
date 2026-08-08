//go:build !windows

package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
)

// DefaultHost is where a Docker daemon listens on this platform.
func DefaultHost() string {
	if fromEnv := os.Getenv("DOCKER_HOST"); fromEnv != "" {
		return fromEnv
	}
	return "unix:///var/run/docker.sock"
}

// dialerFor returns a dialer for a DOCKER_HOST-style address.
func dialerFor(host string) (func(context.Context) (net.Conn, error), error) {
	scheme, address, found := strings.Cut(host, "://")
	if !found {
		return nil, fmt.Errorf("docker: %q is not a host address (expected unix:// or tcp://)", host)
	}

	dialer := &net.Dialer{}

	switch scheme {
	case "unix":
		return func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", address)
		}, nil

	case "tcp", "http":
		return func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address)
		}, nil

	case "npipe":
		// Naming the reason beats "unsupported scheme": someone copying a
		// colleague's DOCKER_HOST across machines needs to know why.
		return nil, fmt.Errorf("docker: named pipes exist only on Windows, but DOCKER_HOST is %q", host)

	default:
		return nil, fmt.Errorf("docker: unsupported host scheme %q", scheme)
	}
}
