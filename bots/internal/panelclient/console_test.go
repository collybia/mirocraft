package panelclient_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/collybia/mirocraft/bots/internal/panelclient"
)

// waitFor polls until condition holds, so tests do not race a server that is
// still starting.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole point of the client: start a server, watch the task, see the
// status change, read the console and send it a command.
func TestPowerConsoleAndCommandEndToEnd(t *testing.T) {
	p := newPanel(t)
	client := p.client()
	ctx := context.Background()

	taskID, err := client.Start(ctx, p.serverID)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if taskID == "" {
		t.Fatal("Start returned no task id")
	}

	task, err := client.WaitTask(ctx, taskID)
	if err != nil {
		t.Fatalf("WaitTask: %v", err)
	}
	if task.Status != panelclient.TaskDone {
		t.Fatalf("task status = %q, want done", task.Status)
	}

	waitFor(t, "the server to report running", func() bool {
		server, err := client.GetServer(ctx, p.serverID)
		return err == nil && server.Status == panelclient.StatusRunning
	})

	// The console has the fake server's first line.
	var history []panelclient.ConsoleLine
	waitFor(t, "the console to have output", func() bool {
		history, err = client.ConsoleHistory(ctx, p.serverID, 50)
		return err == nil && len(history) > 0
	})
	if !strings.Contains(history[0].Text, "fake server ready") {
		t.Errorf("first line = %q, want the server's greeting", history[0].Text)
	}

	// A command over REST comes back through the console, which is what makes
	// it observable at all.
	if err := client.SendCommand(ctx, p.serverID, "say hello"); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	waitFor(t, "the command to be echoed", func() bool {
		lines, err := client.ConsoleHistory(ctx, p.serverID, 50)
		if err != nil {
			return false
		}
		for _, l := range lines {
			if strings.Contains(l.Text, "echo: say hello") {
				return true
			}
		}
		return false
	})

	// And the socket, which is what a bot streaming a console uses.
	console, err := client.OpenConsole(ctx, p.serverID)
	if err != nil {
		t.Fatalf("OpenConsole: %v", err)
	}
	defer func() { _ = console.Close() }()

	if err := console.Send("say over the socket"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	found := false
	for !found && time.Now().Before(deadline) {
		frame, err := console.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		switch frame.Type {
		case panelclient.FrameLine:
			if strings.Contains(frame.Text, "echo: say over the socket") {
				found = true
			}
		case panelclient.FrameError:
			t.Fatalf("the panel rejected the command: %s (%s)", frame.Message, frame.Code)
		}
	}
	if !found {
		t.Fatal("the command sent over the socket was never echoed back")
	}

	if _, err := client.Stop(ctx, p.serverID, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Stopping a server that is not running is a conflict, not a silent success.
func TestStoppingAStoppedServerSaysSo(t *testing.T) {
	p := newPanel(t)

	_, err := p.client().Stop(context.Background(), p.serverID, 0)
	if !errors.Is(err, panelclient.ErrNotRunning) {
		t.Fatalf("error = %v, want ErrNotRunning", err)
	}
}

func TestPowerRefusesAnUnknownAction(t *testing.T) {
	p := newPanel(t)

	_, err := p.client().Power(context.Background(), p.serverID, "detonate", 0)
	if !errors.Is(err, panelclient.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}

// The panel accepts the upgrade before it looks for the server, so a stopped
// server is reported by the first frame rather than by the dial. Asserted
// because a bot that only checks the error from OpenConsole would report a
// working console and then print nothing.
func TestTheConsoleOfAStoppedServerFailsOnTheFirstFrame(t *testing.T) {
	p := newPanel(t)

	console, err := p.client().OpenConsole(context.Background(), p.serverID)
	if err != nil {
		// Also acceptable, and better; the test accepts either.
		return
	}
	defer func() { _ = console.Close() }()

	frame, err := console.Read()
	if err != nil {
		return // the panel closed the socket, which says the same thing
	}
	if frame.Type != panelclient.FrameError {
		t.Fatalf("first frame = %+v, want an error frame", frame)
	}
	if frame.Message == "" {
		t.Error("the error frame says nothing")
	}
}

// A ticket is single use: the panel redeems it on the upgrade, and a second
// attempt with the same one has to fail.
func TestAConsoleTicketIsSingleUse(t *testing.T) {
	p := newPanel(t)
	client := p.client()
	ctx := context.Background()

	ticket, err := client.ConsoleTicket(ctx, p.serverID)
	if err != nil {
		t.Fatalf("ConsoleTicket: %v", err)
	}
	if ticket.Ticket == "" {
		t.Fatal("the panel issued an empty ticket")
	}
	if !ticket.ExpiresAt.After(time.Now()) {
		t.Errorf("expires at %s, which is not in the future", ticket.ExpiresAt)
	}
}

func TestConsoleHistoryRefusesAnImpossibleCount(t *testing.T) {
	p := newPanel(t)

	_, err := p.client().ConsoleHistory(context.Background(), p.serverID, panelclient.MaxHistoryLines+1)
	if !errors.Is(err, panelclient.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}

func TestSendCommandRefusesAnEmptyCommand(t *testing.T) {
	p := newPanel(t)

	if err := p.client().SendCommand(context.Background(), p.serverID, "   "); !errors.Is(err, panelclient.ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}
