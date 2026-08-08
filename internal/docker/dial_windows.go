//go:build windows

package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/Microsoft/go-winio"
)

// DefaultHost is where a Docker daemon listens on this platform.
//
// Docker Desktop is a developer setup rather than a deployment target — the
// documented Windows install runs servers as processes — but a developer on
// Windows should still be able to exercise the Docker path.
func DefaultHost() string {
	if fromEnv := os.Getenv("DOCKER_HOST"); fromEnv != "" {
		return fromEnv
	}
	return `npipe:////./pipe/docker_engine`
}

// dialerFor returns a dialer for a DOCKER_HOST-style address.
func dialerFor(host string) (func(context.Context) (net.Conn, error), error) {
	scheme, address, found := strings.Cut(host, "://")
	if !found {
		return nil, fmt.Errorf("docker: %q is not a host address (expected npipe:// or tcp://)", host)
	}

	switch scheme {
	case "npipe":
		// DOCKER_HOST spells the pipe with forward slashes; the Win32 name
		// wants backslashes, and the leading // of the authority is part of
		// the path here rather than a host.
		pipe := `\\.\pipe\` + strings.TrimPrefix(
			strings.ReplaceAll(strings.TrimPrefix(address, "//"), "/", `\`), `.\pipe\`)
		return func(ctx context.Context) (net.Conn, error) {
			return winio.DialPipeContext(ctx, pipe)
		}, nil

	case "tcp", "http":
		dialer := &net.Dialer{}
		return func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address)
		}, nil

	case "unix":
		return nil, fmt.Errorf("docker: unix sockets are not available on Windows, but DOCKER_HOST is %q", host)

	default:
		return nil, fmt.Errorf("docker: unsupported host scheme %q", scheme)
	}
}
