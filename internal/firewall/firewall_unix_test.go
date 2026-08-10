//go:build !windows

package firewall

import (
	"context"
	"strings"
	"testing"
)

// A machine whose operator switched the firewall off is a machine whose
// operator decided not to have one. Adding a rule would switch it back on.
func TestNothingRunningMeansNothingToDo(t *testing.T) {
	originalRun, originalHas := run, has
	t.Cleanup(func() { run, has = originalRun, originalHas })

	has = func(string) bool { return true }
	run = func(_ context.Context, name string, _ ...string) (string, error) {
		switch name {
		case "ufw":
			return "Status: inactive", nil
		case "firewall-cmd":
			return "not running", nil
		}
		return "", nil
	}

	manager := newManager(discard())
	if _, ok := manager.(Noop); !ok {
		t.Fatalf("manager = %T (%s), want a no-op", manager, manager)
	}
}

func TestUfwIsUsedWhenItIsActive(t *testing.T) {
	originalRun, originalHas := run, has
	t.Cleanup(func() { run, has = originalRun, originalHas })

	has = func(name string) bool { return name == "ufw" }
	run = func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "ufw" {
			return "Status: active", nil
		}
		return "", nil
	}

	if manager := newManager(discard()); manager.String() != "ufw" {
		t.Fatalf("manager = %s, want ufw", manager)
	}
}

func TestUfwOpensAndClosesByPort(t *testing.T) {
	calls := record(t, nil)
	manager := ufw{log: discard()}
	rule := NewRule("01ABC", 25565, false)

	if err := manager.Open(context.Background(), rule); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := manager.Close(context.Background(), rule.Name); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := [][]string{
		{"ufw", "allow", "25565/tcp"},
		{"ufw", "delete", "allow", "25565/tcp"},
	}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %v", *calls)
	}
	for i, call := range *calls {
		if strings.Join(call, " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, call, want[i])
		}
	}
}

func TestFirewalldReloadsAfterChanging(t *testing.T) {
	calls := record(t, nil)
	manager := firewalld{log: discard()}

	if err := manager.Open(context.Background(), NewRule("01ABC", 19132, true)); err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Without the reload the permanent rule is written and not applied, so the
	// port stays shut until something else reloads it — which reads as the
	// panel having done nothing.
	if len(*calls) != 2 || (*calls)[1][1] != "--reload" {
		t.Fatalf("calls = %v", *calls)
	}
	if !strings.Contains(strings.Join((*calls)[0], " "), "--add-port=19132/udp") {
		t.Errorf("first call = %v", (*calls)[0])
	}
}
