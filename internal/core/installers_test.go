package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The parsing these three do is where they can be wrong without anything
// noticing: a version derived incorrectly resolves to a build that exists and
// is for the wrong Minecraft.

func TestMinecraftForNeoForge(t *testing.T) {
	cases := []struct {
		given, want string
		ok          bool
	}{
		{"21.4.147", "1.21.4", true},
		{"20.2.19-beta", "1.20.2", true},
		// A third part of zero is the .0 release, which Minecraft spells
		// without it.
		{"21.0.10", "1.21", true},
		{"0.25w14craftmine.3-beta", "", false},
		{"nonsense", "", false},
		{"21.4", "", false},
	}

	for _, c := range cases {
		t.Run(c.given, func(t *testing.T) {
			got, ok := minecraftForNeoForge(c.given)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if got != c.want {
				t.Errorf("= %q, want %q", got, c.want)
			}
		})
	}
}

func TestSplitPromotion(t *testing.T) {
	cases := []struct {
		given, minecraft, channel string
		ok                        bool
	}{
		{"1.21.4-recommended", "1.21.4", "recommended", true},
		{"1.21.4-latest", "1.21.4", "latest", true},
		// The Minecraft version can carry a dash of its own.
		{"1.7.10_pre4-latest", "1.7.10_pre4", "latest", true},
		{"1.21.4-something", "", "", false},
		{"nonsense", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.given, func(t *testing.T) {
			minecraft, channel, ok := splitPromotion(c.given)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if minecraft != c.minecraft || channel != c.channel {
				t.Errorf("= (%q, %q), want (%q, %q)", minecraft, channel, c.minecraft, c.channel)
			}
		})
	}
}

// Recommended is Forge's own word for the build modpacks are built against;
// latest is merely the newest, and regularly the one with a fresh regression.
func TestForgePrefersRecommended(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"promos": map[string]string{
				"1.21.4-recommended": "54.0.16",
				"1.21.4-latest":      "54.1.0",
				"1.20.1-latest":      "47.3.0",
			},
		})
	}))
	defer server.Close()

	f := NewForge(server.Client())
	f.PromotionsURL = server.URL

	build, err := f.Resolve(context.Background(), "1.21.4")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.Build != "54.0.16" {
		t.Errorf("build = %q, want the recommended one", build.Build)
	}
	if !build.NeedsInstall() {
		t.Error("a forge download is an installer and must say so")
	}

	// A version with only a latest promotion still resolves: refusing would
	// mean offering nothing for versions Forge never promoted.
	build, err = f.Resolve(context.Background(), "1.20.1")
	if err != nil {
		t.Fatalf("Resolve(1.20.1): %v", err)
	}
	if build.Build != "47.3.0" {
		t.Errorf("build = %q, want the latest one", build.Build)
	}
}

// Someone asking for a Minecraft version wants the build people run, not the
// one being tested.
func TestNeoForgePrefersStableOverBeta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []string{"21.4.100", "21.4.147", "21.4.150-beta"},
		})
	}))
	defer server.Close()

	n := NewNeoForge(server.Client())
	n.BaseURL = server.URL

	build, err := n.Resolve(context.Background(), "1.21.4")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.Build != "21.4.147" {
		t.Errorf("build = %q, want the newest stable", build.Build)
	}
}

// A version with nothing but betas still resolves, because a project that has
// published only betas has nothing else to offer.
func TestNeoForgeFallsBackToBeta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []string{"21.9.1-beta", "21.9.2-beta"},
		})
	}))
	defer server.Close()

	n := NewNeoForge(server.Client())
	n.BaseURL = server.URL

	build, err := n.Resolve(context.Background(), "1.21.9")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.Build != "21.9.2-beta" {
		t.Errorf("build = %q, want the newest beta", build.Build)
	}
}

// The launch arguments are read off what the installer actually left, so a
// directory without it must say so rather than produce a command that fails
// with a message about a missing class.
func TestLaunchArgsNeedTheInstallersOutput(t *testing.T) {
	dir := t.TempDir()

	n := NewNeoForge(nil)
	if _, err := n.LaunchArgs(dir, &Build{Version: "1.21.4", Build: "21.4.147"}, TargetLinux); err == nil {
		t.Error("neoforge produced launch arguments with nothing installed")
	}

	q := NewQuilt(nil)
	if _, err := q.LaunchArgs(dir, &Build{Version: "1.21.4"}, TargetLinux); err == nil {
		t.Error("quilt produced launch arguments with nothing installed")
	}

	f := NewForge(nil)
	if _, err := f.LaunchArgs(dir, &Build{Version: "1.21.4", Build: "54.0.16"}, TargetLinux); err == nil {
		t.Error("forge produced launch arguments with nothing installed")
	}
}

// The two systems get different argument files, and which one is needed
// depends on where the server will run — not on where the panel runs. Under
// Docker the server is in a Linux container whatever the host is.
func TestArgsFileFollowsTheTargetSystem(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "libraries", "net", "neoforged", "neoforge", "21.4.147")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"unix_args.txt", "win_args.txt"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("-cp x"), 0o640); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	n := NewNeoForge(nil)
	build := &Build{Version: "1.21.4", Build: "21.4.147"}

	linux, err := n.LaunchArgs(dir, build, TargetLinux)
	if err != nil {
		t.Fatalf("LaunchArgs(linux): %v", err)
	}
	if linux[0] != "@libraries/net/neoforged/neoforge/21.4.147/unix_args.txt" {
		t.Errorf("linux args = %v", linux)
	}

	windows, err := n.LaunchArgs(dir, build, TargetWindows)
	if err != nil {
		t.Fatalf("LaunchArgs(windows): %v", err)
	}
	if windows[0] != "@libraries/net/neoforged/neoforge/21.4.147/win_args.txt" {
		t.Errorf("windows args = %v", windows)
	}
}

// Forge spans the change: before 1.17 its installer leaves a runnable jar,
// after it an argument file. Which is there is checked rather than derived
// from the version, because the installer is the authority.
func TestForgeFallsBackToTheOlderJar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forge-1.16.5-36.2.42.jar"), []byte("x"), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	f := NewForge(nil)
	args, err := f.LaunchArgs(dir, &Build{Version: "1.16.5", Build: "36.2.42"}, TargetLinux)
	if err != nil {
		t.Fatalf("LaunchArgs: %v", err)
	}
	if len(args) != 3 || args[0] != "-jar" || args[1] != "forge-1.16.5-36.2.42.jar" {
		t.Errorf("args = %v, want the older jar invocation", args)
	}
}
