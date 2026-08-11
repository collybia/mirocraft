package backup

import (
	"mime"
	"strings"
	"testing"
	"time"
)

// A Russian server name is the normal case for this panel, and the download
// used to arrive as ________.zip because every non-ASCII rune was replaced.
func TestSuggestNameKeepsLettersOfAnyScript(t *testing.T) {
	at := time.Date(2026, 8, 11, 9, 30, 0, 0, time.UTC)

	cases := map[string]string{
		"Выживание": "Выживание-2026-08-11-0930.zip",
		"Тех-мир 2": "Тех-мир_2-2026-08-11-0930.zip",
		"home":      "home-2026-08-11-0930.zip",
		"":          "server-2026-08-11-0930.zip",
		"...":       "server-2026-08-11-0930.zip",
	}
	for name, want := range cases {
		if got := SuggestName(name, at); got != want {
			t.Errorf("SuggestName(%q) = %q, want %q", name, got, want)
		}
	}

	// The header is what a browser actually reads, and it has to survive the
	// trip: mime encodes what cannot be sent raw.
	header := mime.FormatMediaType("attachment",
		map[string]string{"filename": SuggestName("Выживание", at)})
	if !strings.Contains(header, "utf-8''") {
		t.Errorf("Content-Disposition = %q, want an RFC 2231 encoded name", header)
	}
}

// sanitize guards a real path join, so it stays strict where SuggestName was
// loosened. Ids are ASCII, and anything that could climb out of the backup
// directory is flattened rather than escaped.
func TestSanitizeKeepsPathsFlat(t *testing.T) {
	cases := map[string]string{
		"01KZQNDW1R3QKGNRV5SNFV1ZWY": "01KZQNDW1R3QKGNRV5SNFV1ZWY",
		"../../etc/passwd":           "______etc_passwd",
		`..\..\windows`:              "______windows",
		"Выживание":                  "_________",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
