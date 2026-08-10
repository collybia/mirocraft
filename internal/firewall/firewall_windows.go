package firewall

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// newManager returns the Windows Firewall backend.
//
// Always, and without checking whether the firewall is switched on: on Windows
// it is on by default, an operator who turned it off loses nothing by having a
// rule added anyway, and asking costs a process launch on every server start.
func newManager(log *slog.Logger) Manager { return windowsFirewall{log: log} }

// windowsFirewall drives netsh.
//
// netsh rather than the PowerShell cmdlets: it is present on every edition
// including Core, it needs no module loading, and it does not care which
// language the group names are in — New-NetFirewallRule would be another
// process and another dependency for the same three lines.
type windowsFirewall struct{ log *slog.Logger }

func (w windowsFirewall) String() string { return "windows firewall" }

func (w windowsFirewall) Open(ctx context.Context, rule Rule) error {
	// Removed first so a port that changed does not leave the old rule behind
	// under the same name, and so this stays safe to call on every start.
	_ = w.Close(ctx, rule.Name)

	out, err := run(ctx, "netsh", "advfirewall", "firewall", "add", "rule",
		"name="+rule.Name,
		"dir=in", "action=allow",
		"protocol="+strings.ToUpper(rule.Protocol()),
		"localport="+strconv.Itoa(rule.Port))
	if err != nil {
		return fmt.Errorf("adding a firewall rule for port %d: %w: %s", rule.Port, err, out)
	}

	w.log.Info("firewall rule added",
		slog.String("rule", rule.Name),
		slog.Int("port", rule.Port),
		slog.String("protocol", rule.Protocol()))
	return nil
}

func (w windowsFirewall) Close(ctx context.Context, name string) error {
	// netsh reports "No rules match" as a failure. Nothing to remove is the
	// outcome asked for, so it is not one — and there is no way to tell that
	// case from a real error except by its exit code, which netsh spends on
	// both.
	if _, err := run(ctx, "netsh", "advfirewall", "firewall", "delete", "rule", "name="+name); err != nil {
		return nil //nolint:nilerr // nothing to delete is success here
	}
	w.log.Info("firewall rule removed", slog.String("rule", name))
	return nil
}
