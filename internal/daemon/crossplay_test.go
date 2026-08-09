package daemon

import (
	"strings"
	"testing"
)

// The real file Geyser 2.11 writes, cut to the parts that matter. Taken from a
// server the panel actually ran rather than from memory: an earlier version of
// this code looked for a "remote:" section, which does not exist in this
// release, and so quietly changed nothing.
const geyserSample = `# --------------------------------
# Geyser Configuration File
# --------------------------------
# Network settings for the Bedrock listener
bedrock:
  address: 0.0.0.0
  port: 19132
  clone-remote-port: false
# Network settings for the Java server connection
java:
  auth-type: online
  address: auto
  port: 25565
# Unrelated
max-visible-custom-skulls: 128
`

func TestGeyserPortIsApplied(t *testing.T) {
	updated, changed := rewriteGeyserConfig(geyserSample, 19140, true)
	if !changed {
		t.Fatal("the configuration was left alone")
	}

	if !strings.Contains(updated, "  port: 19140") {
		t.Errorf("the bedrock port was not applied:\n%s", updated)
	}
	// The java section has a port too, and it is this server's own. Changing
	// it would point Geyser at itself, which fails as a connection timeout
	// with nothing in either log to explain it.
	if !strings.Contains(updated, "  port: 25565") {
		t.Errorf("the java port was changed and should not have been:\n%s", updated)
	}
	// Comments and unrelated keys are the operator's.
	if !strings.Contains(updated, "# Network settings for the Bedrock listener") {
		t.Errorf("comments were lost:\n%s", updated)
	}
	if !strings.Contains(updated, "max-visible-custom-skulls: 128") {
		t.Errorf("an unrelated key was lost:\n%s", updated)
	}
}

// The setting that decides whether crossplay works at all: left at "online",
// Geyser asks a Bedrock player for a Java account, which is what Floodgate
// was installed to avoid.
func TestFloodgateSwitchesTheAuthType(t *testing.T) {
	updated, changed := rewriteGeyserConfig(geyserSample, 19132, true)
	if !changed {
		t.Fatal("the configuration was left alone")
	}
	if !strings.Contains(updated, "auth-type: floodgate") {
		t.Errorf("the auth type was not switched:\n%s", updated)
	}
}

// Without Floodgate the auth type stays as it is: a server whose players do
// have Java accounts is a working setup, and switching it would lock them out.
func TestWithoutFloodgateTheAuthTypeIsLeftAlone(t *testing.T) {
	updated, _ := rewriteGeyserConfig(geyserSample, 19140, false)

	if !strings.Contains(updated, "auth-type: online") {
		t.Errorf("the auth type was changed without floodgate:\n%s", updated)
	}
}

// Run before every start, so a file that already agrees must be left alone.
func TestGeyserConfigIsNotRewrittenNeedlessly(t *testing.T) {
	first, _ := rewriteGeyserConfig(geyserSample, 19140, true)

	if _, changed := rewriteGeyserConfig(first, 19140, true); changed {
		t.Error("the configuration was rewritten although it already agreed")
	}
}

// Windows line endings survive: a config rewritten with the wrong ones is a
// diff nobody can read.
func TestGeyserConfigKeepsItsShape(t *testing.T) {
	updated, changed := rewriteGeyserConfig(strings.ReplaceAll(geyserSample, "\n", "\r\n"), 19140, true)
	if !changed {
		t.Fatal("the configuration was left alone")
	}
	if !strings.Contains(updated, "port: 19140") {
		t.Errorf("a file with CRLF endings was not understood:\n%s", updated)
	}
}
