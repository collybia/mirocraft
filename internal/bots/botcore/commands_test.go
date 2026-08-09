package botcore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/bots/botcore"
	"github.com/collybia/mirocraft/internal/panelclient"
)

// The commands are exercised against the real panel through the real client,
// for the same reason the client is: what has to hold is that the three agree,
// and a stub only proves the test author was consistent with themselves.

const discordUser = "31337"

func TestServersListsWhatThePersonOwns(t *testing.T) {
	env := newEnv(t)
	env.link(t, discordUser)

	reply := env.cmd.Servers(context.Background(), discordUser)
	if !strings.Contains(reply.Text, "survival") {
		t.Fatalf("reply = %q, want the server's name", reply.Text)
	}
	if !reply.Monospace {
		t.Error("a table is rendered without a code block, so the columns will not line up")
	}
}

// Someone who has not linked gets told what to do about it, not "forbidden".
func TestAnUnlinkedPersonIsToldHowToLink(t *testing.T) {
	env := newEnv(t)

	reply := env.cmd.Servers(context.Background(), "99999")
	if !strings.Contains(reply.Text, "/link") {
		t.Fatalf("reply = %q, want instructions for linking", reply.Text)
	}
	if !strings.Contains(reply.Text, env.panelURL) {
		t.Errorf("reply = %q, want the panel's address", reply.Text)
	}
	if !reply.Ephemeral {
		t.Error("a refusal is shown to the whole channel")
	}
}

func TestLinkAndUnlink(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	code := env.issueCode(t)
	reply := env.cmd.Link(ctx, discordUser, code)
	if !strings.Contains(reply.Text, testEmail) {
		t.Fatalf("reply = %q, want the account it linked to", reply.Text)
	}

	// And now the commands work.
	if servers := env.cmd.Servers(ctx, discordUser); !strings.Contains(servers.Text, "survival") {
		t.Fatalf("after linking, /servers said %q", servers.Text)
	}

	if reply := env.cmd.Unlink(ctx, discordUser); !strings.Contains(reply.Text, "твязал") {
		t.Fatalf("unlink said %q", reply.Text)
	}
	if servers := env.cmd.Servers(ctx, discordUser); !strings.Contains(servers.Text, "/link") {
		t.Fatalf("after unlinking, /servers said %q", servers.Text)
	}
}

// A wrong code is the commonest mistake, so the answer has to say what to do
// rather than that something was invalid.
func TestABadCodeExplainsItself(t *testing.T) {
	env := newEnv(t)

	reply := env.cmd.Link(context.Background(), discordUser, "ZZZZ-ZZZZ")
	if !strings.Contains(reply.Text, "десять минут") {
		t.Fatalf("reply = %q, want an explanation of why the code failed", reply.Text)
	}
	if !strings.Contains(reply.Text, env.panelURL) {
		t.Errorf("reply = %q, want somewhere to get a new one", reply.Text)
	}
}

// A code works once, and the second attempt has to read like the first
// succeeded rather than like something broke.
func TestASpentCodeIsRefused(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	code := env.issueCode(t)
	if reply := env.cmd.Link(ctx, discordUser, code); !strings.Contains(reply.Text, testEmail) {
		t.Fatalf("first link: %q", reply.Text)
	}
	if reply := env.cmd.Link(ctx, "42", code); !strings.Contains(reply.Text, "Код не подошёл") {
		t.Fatalf("second link: %q", reply.Text)
	}
}

func TestPowerAndStatus(t *testing.T) {
	env := newEnv(t)
	env.link(t, discordUser)
	ctx := context.Background()

	reply := env.cmd.Power(ctx, discordUser, "survival", panelclient.ActionStart)
	if !strings.Contains(reply.Text, "Запускаю") {
		t.Fatalf("start said %q", reply.Text)
	}

	// The status has to reflect it, once the daemon has caught up.
	deadline := time.Now().Add(10 * time.Second)
	var status botcore.Reply
	for time.Now().Before(deadline) {
		status = env.cmd.Status(ctx, discordUser, "survival")
		if strings.Contains(status.Text, "запущен") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(status.Text, "запущен") {
		t.Fatalf("status said %q, want a running server", status.Text)
	}
	if !strings.Contains(status.Text, "paper") {
		t.Errorf("status = %q, want the core", status.Text)
	}

	// A command reaches the server and its echo lands in the console.
	if reply := env.cmd.Command(ctx, discordUser, "survival", "say hello"); !strings.Contains(reply.Text, "Отправил") {
		t.Fatalf("cmd said %q", reply.Text)
	}

	deadline = time.Now().Add(10 * time.Second)
	var console botcore.Reply
	for time.Now().Before(deadline) {
		console = env.cmd.Console(ctx, discordUser, "survival", 20)
		if strings.Contains(console.Text, "echo: say hello") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(console.Text, "echo: say hello") {
		t.Fatalf("console = %q, want the echoed command", console.Text)
	}

	if reply := env.cmd.Power(ctx, discordUser, "survival", panelclient.ActionStop); !strings.Contains(reply.Text, "станавливаю") {
		t.Fatalf("stop said %q", reply.Text)
	}
}

// Stopping something that is not running is a mistake worth naming.
func TestStoppingAStoppedServerSaysWhichServer(t *testing.T) {
	env := newEnv(t)
	env.link(t, discordUser)

	reply := env.cmd.Power(context.Background(), discordUser, "survival", panelclient.ActionStop)
	if !strings.Contains(reply.Text, "не запущен") {
		t.Fatalf("reply = %q, want a plain refusal", reply.Text)
	}
	if !strings.Contains(reply.Text, "survival") {
		t.Errorf("reply = %q, want the server named", reply.Text)
	}
}

func TestAnUnknownServerNameSuggestsTheList(t *testing.T) {
	env := newEnv(t)
	env.link(t, discordUser)

	reply := env.cmd.Status(context.Background(), discordUser, "nothing-like-it")
	if !strings.Contains(reply.Text, "/servers") {
		t.Fatalf("reply = %q, want a pointer at the list", reply.Text)
	}
}

// Two servers matching one word must not be resolved by guessing.
func TestAnAmbiguousNameIsRefusedWithTheCandidates(t *testing.T) {
	env := newEnv(t)
	env.link(t, discordUser)
	env.createServer(t, "survival-one")
	env.createServer(t, "survival-two")

	reply := env.cmd.Power(context.Background(), discordUser, "survival-", panelclient.ActionStart)
	if !strings.Contains(reply.Text, "survival-one") || !strings.Contains(reply.Text, "survival-two") {
		t.Fatalf("reply = %q, want both candidates named", reply.Text)
	}
}

func TestEmptyCommandIsRefused(t *testing.T) {
	env := newEnv(t)
	env.link(t, discordUser)

	reply := env.cmd.Command(context.Background(), discordUser, "survival", "   ")
	if !strings.Contains(reply.Text, "пустая") {
		t.Fatalf("reply = %q", reply.Text)
	}
}
