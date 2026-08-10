//go:build !windows

package firewall

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// newManager picks the firewall this machine actually runs.
//
// Only one that is switched on is touched. A machine with ufw installed and
// disabled is a machine whose operator decided not to have a firewall, and
// enabling one on their behalf — even by adding an allow rule — is a change
// nobody asked for. Where there is nothing running, there is nothing to open,
// and the honest answer is to say so rather than to pretend.
func newManager(log *slog.Logger) Manager {
	ctx := context.Background()

	if has("ufw") {
		if out, err := run(ctx, "ufw", "status"); err == nil && strings.Contains(out, "Status: active") {
			return ufw{log: log}
		}
	}
	if has("firewall-cmd") {
		// Equality, not Contains: firewalld answers "not running" when it is
		// stopped, and that contains "running". The check would have said yes
		// to every machine where firewalld is installed and switched off.
		if out, err := run(ctx, "firewall-cmd", "--state"); err == nil && strings.TrimSpace(out) == "running" {
			return firewalld{log: log}
		}
	}
	return Noop{Reason: "no firewall is running on this host"}
}

// --- ufw ---

type ufw struct{ log *slog.Logger }

func (u ufw) String() string { return "ufw" }

func (u ufw) Open(ctx context.Context, rule Rule) error {
	port := strconv.Itoa(rule.Port) + "/" + rule.Protocol()

	// ufw is idempotent by itself: a second allow for the same port reports
	// "Skipping adding existing rule" and succeeds.
	out, err := run(ctx, "ufw", "allow", port)
	if err != nil {
		return fmt.Errorf("ufw allow %s: %w: %s", port, err, out)
	}

	u.log.Info("firewall rule added",
		slog.String("backend", "ufw"), slog.Int("port", rule.Port),
		slog.String("protocol", rule.Protocol()))
	return nil
}

// Close removes the rule by port, because that is the only handle ufw offers:
// its rules are not named, so the name is parsed back into what it describes.
func (u ufw) Close(ctx context.Context, name string) error {
	port, proto, ok := portFromName(name)
	if !ok {
		return nil
	}
	if _, err := run(ctx, "ufw", "delete", "allow", strconv.Itoa(port)+"/"+proto); err != nil {
		// A rule that is not there is the outcome asked for.
		return nil
	}
	u.log.Info("firewall rule removed", slog.String("backend", "ufw"), slog.Int("port", port))
	return nil
}

// --- firewalld ---

type firewalld struct{ log *slog.Logger }

func (f firewalld) String() string { return "firewalld" }

func (f firewalld) Open(ctx context.Context, rule Rule) error {
	port := strconv.Itoa(rule.Port) + "/" + rule.Protocol()

	if out, err := run(ctx, "firewall-cmd", "--permanent", "--add-port="+port); err != nil {
		return fmt.Errorf("firewall-cmd --add-port=%s: %w: %s", port, err, out)
	}
	if out, err := run(ctx, "firewall-cmd", "--reload"); err != nil {
		return fmt.Errorf("firewall-cmd --reload: %w: %s", err, out)
	}

	f.log.Info("firewall rule added",
		slog.String("backend", "firewalld"), slog.Int("port", rule.Port),
		slog.String("protocol", rule.Protocol()))
	return nil
}

func (f firewalld) Close(ctx context.Context, name string) error {
	port, proto, ok := portFromName(name)
	if !ok {
		return nil
	}
	if _, err := run(ctx, "firewall-cmd", "--permanent", "--remove-port="+strconv.Itoa(port)+"/"+proto); err != nil {
		return nil
	}
	_, _ = run(ctx, "firewall-cmd", "--reload")
	f.log.Info("firewall rule removed", slog.String("backend", "firewalld"), slog.Int("port", port))
	return nil
}
