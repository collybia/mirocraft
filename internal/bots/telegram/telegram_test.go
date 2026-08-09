package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/collybia/mirocraft/internal/bots/botcore"
)

// Telegram hands a command's arguments over as one string, so the splitting is
// this package's job and its own to get wrong.
func TestSplitFirst(t *testing.T) {
	cases := []struct {
		given      string
		name, rest string
	}{
		{"", "", ""},
		{"   ", "", ""},
		{"survival", "survival", ""},
		{"survival say hello", "survival", "say hello"},
		{"  survival   say  hello  ", "survival", "say  hello"},
		// A command with several words keeps them: "say hello world" is one
		// command, not three arguments.
		{"survival say hello world", "survival", "say hello world"},
	}

	for _, c := range cases {
		t.Run(c.given, func(t *testing.T) {
			name, rest := splitFirst(c.given)
			if name != c.name || rest != c.rest {
				t.Errorf("splitFirst(%q) = (%q, %q), want (%q, %q)", c.given, name, rest, c.name, c.rest)
			}
		})
	}
}

// A Minecraft log contains backticks and backslashes, and MarkdownV2 reads
// both inside a code fence. Unescaped, a stack trace turns the rest of the
// message into markup and Telegram rejects it outright.
func TestFencedOutputIsEscaped(t *testing.T) {
	reply := botcore.Reply{
		Text:      "at java.base/java.lang.Thread.run(Thread.java:833) `tick` C:\\srv",
		Monospace: true,
	}

	rendered := render(reply)
	if !strings.HasPrefix(rendered, "```") || !strings.HasSuffix(rendered, "```") {
		t.Fatalf("rendered = %q, want a code fence", rendered)
	}
	if strings.Contains(rendered, "` ") && !strings.Contains(rendered, "\\`") {
		t.Errorf("a backtick survived unescaped: %q", rendered)
	}
	if !strings.Contains(rendered, "\\\\srv") {
		t.Errorf("a backslash survived unescaped: %q", rendered)
	}
}

// Telegram refuses a message over its limit, so a long console has to be cut
// rather than sent and rejected.
func TestLongOutputIsTruncated(t *testing.T) {
	line := "[12:00:00] [Server thread/INFO]: something happened on the server\n"
	reply := botcore.Reply{Text: strings.Repeat(line, 200), Monospace: true}

	rendered := render(reply)
	if len(rendered) > messageLimit+16 {
		t.Fatalf("rendered %d bytes, over the limit of %d", len(rendered), messageLimit)
	}
	if !strings.Contains(rendered, "обрезано") {
		t.Error("the message was cut without saying so")
	}
}

// Cutting in the middle of a multi-byte character leaves a replacement glyph,
// which looks like corruption rather than truncation. Cyrillic is two bytes a
// character, so a byte limit lands mid-character often.
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

// A bare /start is Telegram's own greeting, not a request to start a server
// that was never named.
func TestBareStartIsAGreeting(t *testing.T) {
	bot := &Bot{}

	reply := bot.greeting()
	if !strings.Contains(reply.Text, "/link") {
		t.Errorf("greeting = %q, want the first thing a new user has to do", reply.Text)
	}
	if !strings.Contains(reply.Text, "/servers") {
		t.Errorf("greeting = %q, want the commands", reply.Text)
	}
}
