package firewall

import (
	"context"
	"strings"
	"testing"
)

// The rule a Minecraft server needs, and the shape netsh expects it in.
//
// Worth pinning: netsh takes name=value pairs, answers a malformed one by
// printing its usage and returning a non-zero code, and there is nothing in
// that output to suggest which pair was wrong.
func TestWindowsAddsAndRemovesByName(t *testing.T) {
	calls := record(t, nil)
	manager := windowsFirewall{log: discard()}
	rule := NewRule("01ABC", 25565, false)

	if err := manager.Open(context.Background(), rule); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Two calls: the delete that makes this safe to run on every start, then
	// the add.
	if len(*calls) != 2 {
		t.Fatalf("calls = %v", *calls)
	}
	add := strings.Join((*calls)[1], " ")
	for _, want := range []string{
		"netsh advfirewall firewall add rule",
		"name=" + rule.Name,
		"dir=in", "action=allow", "protocol=TCP", "localport=25565",
	} {
		if !strings.Contains(add, want) {
			t.Errorf("the add call has no %q: %s", want, add)
		}
	}

	*calls = nil
	if err := manager.Close(context.Background(), rule.Name); err != nil {
		t.Fatalf("Close: %v", err)
	}
	remove := strings.Join((*calls)[0], " ")
	if !strings.Contains(remove, "delete rule") || !strings.Contains(remove, "name="+rule.Name) {
		t.Errorf("the delete call = %s", remove)
	}
}

// A Bedrock or Geyser port is UDP, and a rule that opened TCP instead would
// leave the panel reporting success and Bedrock clients seeing nothing.
func TestWindowsUsesTheRightProtocol(t *testing.T) {
	calls := record(t, nil)
	manager := windowsFirewall{log: discard()}

	if err := manager.Open(context.Background(), NewRule("01ABC", 19132, true)); err != nil {
		t.Fatalf("Open: %v", err)
	}

	add := strings.Join((*calls)[1], " ")
	if !strings.Contains(add, "protocol=UDP") || !strings.Contains(add, "localport=19132") {
		t.Errorf("add call = %s", add)
	}
}
