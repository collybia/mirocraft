package firewall

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// record replaces the command runner, so a test can see what would have been
// run without letting the suite reconfigure the machine it runs on.
func record(t *testing.T, answers map[string]string) *[][]string {
	t.Helper()

	var calls [][]string
	originalRun, originalHas := run, has
	t.Cleanup(func() { run, has = originalRun, originalHas })

	run = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		return answers[name+" "+strings.Join(args, " ")], nil
	}
	has = func(string) bool { return true }
	return &calls
}

// The name is the only handle the Unix tools give, so it has to carry enough
// to find the rule again.
func TestRuleNameCarriesThePortAndProtocol(t *testing.T) {
	tcp := NewRule("01ABC", 25565, false)
	if !strings.Contains(tcp.Name, "01ABC") || !strings.Contains(tcp.Name, "25565/tcp") {
		t.Errorf("tcp rule name = %q", tcp.Name)
	}

	udp := NewRule("01ABC", 19132, true)
	if !strings.Contains(udp.Name, "19132/udp") {
		t.Errorf("udp rule name = %q", udp.Name)
	}
	if tcp.Name == udp.Name {
		t.Error("a tcp and a udp rule share a name, so one would remove the other")
	}
}

func TestPortIsReadBackFromTheName(t *testing.T) {
	for _, rule := range []Rule{
		NewRule("01ABC", 25565, false),
		NewRule("01ABC", 19132, true),
		NewRule("server-with-(brackets)", 25570, false),
	} {
		port, proto, ok := portFromName(rule.Name)
		if !ok {
			t.Errorf("%q was not understood", rule.Name)
			continue
		}
		if port != rule.Port || proto != rule.Protocol() {
			t.Errorf("%q -> %d/%s, want %d/%s", rule.Name, port, proto, rule.Port, rule.Protocol())
		}
	}

	if _, _, ok := portFromName("something else entirely"); ok {
		t.Error("a name this package did not write was parsed anyway")
	}
	if _, _, ok := portFromName("Mirocraft server x (99999/tcp)"); ok {
		t.Error("a port outside the range was accepted")
	}
}
