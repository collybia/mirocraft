package discord

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/collybia/mirocraft/internal/bots/botcore"
)

// Discord refuses a message over two thousand characters, so a long console
// has to be cut rather than sent and rejected. This is the sort of thing that
// works on every message anyone sends while developing and fails on the first
// real crash log.
func TestLongOutputIsTruncated(t *testing.T) {
	line := "[12:00:00] [Server thread/INFO]: something happened on the server\n"
	reply := botcore.Reply{Text: strings.Repeat(line, 200), Monospace: true}

	rendered := render(reply)
	if len(rendered) > 2000 {
		t.Fatalf("rendered %d bytes, over Discord's limit", len(rendered))
	}
	if !strings.Contains(rendered, "обрезано") {
		t.Error("the message was cut without saying so")
	}
	if !strings.HasPrefix(rendered, "```") || !strings.HasSuffix(rendered, "```") {
		t.Errorf("the code fence did not survive truncation: %q", rendered[:40])
	}
}

// Cyrillic is two bytes a character, so a byte limit lands mid-character
// often, and the result is a replacement glyph that reads as corruption.
func TestTruncationDoesNotSplitCharacters(t *testing.T) {
	text := strings.Repeat("сервер запущен и работает\n", 500)

	for limit := 990; limit < 1010; limit++ {
		cut := truncate(text, limit)
		if !utf8.ValidString(cut) {
			t.Fatalf("truncate at %d produced invalid utf-8", limit)
		}
		if strings.ContainsRune(cut, utf8.RuneError) {
			t.Fatalf("truncate at %d left a replacement character", limit)
		}
	}
}

// A short reply passes through untouched: the truncation must not leave its
// mark on messages that fit.
func TestShortRepliesAreLeftAlone(t *testing.T) {
	reply := botcore.Reply{Text: "Запускаю survival."}

	if got := render(reply); got != reply.Text {
		t.Errorf("render = %q, want it unchanged", got)
	}
}

// Every command Discord is told about has to be one dispatch answers, or a
// player gets "such a command does not exist" from a command in the menu.
func TestEveryRegisteredCommandIsHandled(t *testing.T) {
	bot := &Bot{commands: &botcore.Commands{}}
	handlers := bot.handlers()

	for _, definition := range definitions() {
		if _, ok := handlers[definition.Name]; !ok {
			t.Errorf("the command %q is offered in Discord's menu and not handled", definition.Name)
		}
	}

	// And the other way: a handler nobody can invoke is dead code that reads
	// like a working command.
	registered := make(map[string]bool, len(definitions()))
	for _, definition := range definitions() {
		registered[definition.Name] = true
	}
	for name := range handlers {
		if !registered[name] {
			t.Errorf("the command %q is handled and never offered to anyone", name)
		}
	}
}
