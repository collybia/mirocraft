// Package firewall opens the ports the panel's servers listen on.
//
// A panel that starts a server and leaves it unreachable has done half a job.
// Windows Firewall is on by default and blocks every inbound port nobody asked
// for; a Linux box with ufw enabled does the same. In both cases the panel
// reports a running server, the operator hands out an address, and nothing
// connects — with nothing in any log to explain it, because from the server's
// side nothing is wrong. That was found on a real Windows Server, with a real
// Paper server listening and refusing the world.
//
// What this does not do: turn a firewall on, touch a rule it did not create,
// or open anything but the port of a server the operator asked for.
package firewall

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// Rule is one port to let through.
type Rule struct {
	// Name identifies the rule so it can be removed again. It carries the
	// server id, because a rule nobody can attribute is a rule nobody dares
	// delete.
	Name string
	Port int
	// UDP marks a Bedrock or Geyser port. Java is TCP.
	UDP bool
}

// Protocol returns the name the host's tooling expects.
func (r Rule) Protocol() string {
	if r.UDP {
		return "udp"
	}
	return "tcp"
}

// Manager opens and closes ports.
type Manager interface {
	// Open lets a port through. It is called on every start, so it has to be
	// safe to call for a rule that already exists.
	Open(ctx context.Context, rule Rule) error
	// Close removes a rule this package created. Missing is success: the
	// operator may have removed it by hand, and that is their right.
	Close(ctx context.Context, name string) error
	// Describes the backend, for the log line that says what is managing the
	// firewall — or that nothing is.
	String() string
}

// NewRule describes the port of one server.
//
// The name carries the port and the protocol because the Unix tools have no
// named rules: there the name is all the caller holds, and it has to be enough
// to find the rule again.
func NewRule(serverID string, port int, udp bool) Rule {
	rule := Rule{Port: port, UDP: udp}
	rule.Name = fmt.Sprintf("Mirocraft server %s (%d/%s)", serverID, port, rule.Protocol())
	return rule
}

// New returns the manager for this host, or a no-op when there is no firewall
// to manage.
func New(log *slog.Logger) Manager {
	if log == nil {
		log = slog.Default()
	}
	return newManager(log)
}

// Noop does nothing, for a host with no firewall and for an operator who
// switched the feature off.
type Noop struct{ Reason string }

// Open does nothing and says it worked, which it did: there was nothing to do.
func (n Noop) Open(context.Context, Rule) error { return nil }

// Close does nothing, for the same reason.
func (n Noop) Close(context.Context, string) error { return nil }
func (n Noop) String() string {
	if n.Reason == "" {
		return "none"
	}
	return "none (" + n.Reason + ")"
}

// run executes a command and returns its combined output on failure.
//
// A variable so the tests can watch what would have been run without letting a
// test suite reconfigure the machine it runs on.
var run = func(ctx context.Context, name string, args ...string) (string, error) {
	// The command and its arguments are built by this package from a port
	// number and a server id, never from user input.
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- arguments are constructed here
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// has reports whether a command exists on this host.
var has = func(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// portFromName reads back what RuleName wrote.
//
// The Unix tools have no concept of a named rule, so the name has to carry the
// port and the name is what the caller holds. Parsing a string this package
// formatted itself is not elegant, and the alternative — a second store of
// rule-to-port mappings that can disagree with the firewall — is worse.
func portFromName(name string) (int, string, bool) {
	open := strings.LastIndex(name, "(")
	closing := strings.LastIndex(name, ")")
	if open < 0 || closing < open {
		return 0, "", false
	}
	body := name[open+1 : closing]

	proto := "tcp"
	if slash := strings.Index(body, "/"); slash >= 0 {
		proto = body[slash+1:]
		body = body[:slash]
	}

	port, err := strconv.Atoi(strings.TrimSpace(body))
	if err != nil || port <= 0 || port > 65535 {
		return 0, "", false
	}
	return port, proto, true
}
