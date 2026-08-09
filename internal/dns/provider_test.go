package dns

import (
	"net/netip"
	"strings"
	"testing"
)

func TestValidateSub(t *testing.T) {
	valid := []string{"", "mc", "survival", "a", "my-server", "a1", "deep.name"}
	for _, sub := range valid {
		if err := ValidateSub(sub); err != nil {
			t.Errorf("ValidateSub(%q) = %v, want it accepted", sub, err)
		}
	}

	// The value reaches an upstream URL and a record name, so the panel checks
	// it rather than finding out what each API does with a semicolon.
	invalid := []string{
		"MC",          // uppercase
		"-leading",    //
		"trailing-",   //
		"has space",   //
		"semi;colon",  //
		"../escape",   //
		"under_score", // not a hostname character
		"a..b",        // empty label
		strings.Repeat("x", 64),
	}
	for _, sub := range invalid {
		if err := ValidateSub(sub); err == nil {
			t.Errorf("ValidateSub(%q) was accepted", sub)
		}
	}
}

func TestFQDN(t *testing.T) {
	if got := FQDN("", "example.com"); got != "example.com" {
		t.Errorf("apex = %q", got)
	}
	if got := FQDN("mc", "example.com"); got != "mc.example.com" {
		t.Errorf("sub = %q", got)
	}
}

func TestRecordTypeFollowsTheFamily(t *testing.T) {
	v4 := netip.MustParseAddr("203.0.113.7")
	v6 := netip.MustParseAddr("2001:db8::1")

	if got := RecordType(v4); got != "A" {
		t.Errorf("v4 = %q", got)
	}
	// A v6-only host is a real deployment, and writing an A record for it
	// would publish nothing usable.
	if got := RecordType(v6); got != "AAAA" {
		t.Errorf("v6 = %q", got)
	}
}

// A target without the trailing dot is completed by the resolver with the
// zone, so mc.example.com becomes mc.example.com.example.com — which resolves
// to nothing and looks exactly like a server that is down.
func TestSRVValueEndsWithADot(t *testing.T) {
	got := SRVValue("mc.example.com", 25566)
	if got != "0 5 25566 mc.example.com." {
		t.Fatalf("SRVValue = %q", got)
	}

	// A target that already ends in a dot must not gain a second one.
	if got := SRVValue("mc.example.com.", 25566); got != "0 5 25566 mc.example.com." {
		t.Fatalf("SRVValue with a dot = %q", got)
	}
}

func TestEffectiveTTL(t *testing.T) {
	cases := []struct{ requested, floor, want int }{
		{0, 0, DefaultTTL}, // unset
		{300, 0, 300},      // no floor
		{300, 3600, 3600},  // below the floor is raised
		{7200, 3600, 7200}, // above it is kept
		// Nonsense falls back to the default, which is then floored like any
		// other value — so a low floor leaves the default alone and a high one
		// raises it.
		{-5, 60, DefaultTTL},
		{-5, 3600, 3600},
	}
	for _, c := range cases {
		if got := effectiveTTL(c.requested, c.floor); got != c.want {
			t.Errorf("effectiveTTL(%d, %d) = %d, want %d", c.requested, c.floor, got, c.want)
		}
	}
}

func TestRegistryBuildsTheKnownProviders(t *testing.T) {
	r := NewRegistry()

	if len(r.List()) != 3 {
		t.Fatalf("the registry holds %d providers", len(r.List()))
	}

	for _, entry := range r.List() {
		if entry.Name == "" || entry.Description == "" {
			t.Errorf("%s has no name or description for the installer to show", entry.ID)
		}
	}

	if _, err := r.Build("nonsense", Config{}); err == nil {
		t.Error("an unknown provider was built")
	}
}

// Each provider refuses to be built without what it needs, rather than failing
// later with something obscure from upstream.
func TestProvidersRefuseIncompleteConfiguration(t *testing.T) {
	r := NewRegistry()

	cases := []struct {
		id  string
		cfg Config
	}{
		{"desec", Config{Token: "t"}},                        // no domain
		{"desec", Config{Zone: "x.dedyn.io"}},                // no token
		{"duckdns", Config{Token: "t"}},                      // no name
		{"duckdns", Config{Zone: "myname"}},                  // no token
		{"duckdns", Config{Zone: "a.b.example", Token: "t"}}, // not a DuckDNS name
		{"cloudflare", Config{Token: "t"}},                   // no zone
		{"cloudflare", Config{Zone: "example.com"}},          // no token
	}

	for _, c := range cases {
		if _, err := r.Build(c.id, c.cfg); err == nil {
			t.Errorf("%s accepted %+v", c.id, c.cfg)
		}
	}
}

func TestDuckDNSAcceptsEitherFormOfTheName(t *testing.T) {
	// The site shows "myserver", the browser shows the full name, and an
	// operator will paste whichever is in front of them.
	for _, zone := range []string{"myserver", "myserver.duckdns.org", "MyServer.DuckDNS.org"} {
		provider, err := NewDuckDNS(Config{Zone: zone, Token: "t"}, nil)
		if err != nil {
			t.Fatalf("NewDuckDNS(%q): %v", zone, err)
		}
		if provider.Zone() != "myserver.duckdns.org" {
			t.Errorf("NewDuckDNS(%q).Zone() = %q", zone, provider.Zone())
		}
	}
}

func TestCapabilitiesAreHonest(t *testing.T) {
	desec, err := NewDeSEC(Config{Zone: "x.dedyn.io", Token: "t"}, nil)
	if err != nil {
		t.Fatalf("deSEC: %v", err)
	}
	duck, err := NewDuckDNS(Config{Zone: "myname", Token: "t"}, nil)
	if err != nil {
		t.Fatalf("DuckDNS: %v", err)
	}

	if !desec.Capabilities().SRV {
		t.Error("deSEC reports no SRV support, but that is why it is the default")
	}
	// The whole reason the capability exists: a panel that claimed otherwise
	// would publish a server on a non-standard port that nobody can reach by
	// name, with nothing looking broken.
	if duck.Capabilities().SRV {
		t.Error("DuckDNS reports SRV support, which it does not have")
	}
	if duck.Capabilities().Subdomains {
		t.Error("DuckDNS reports subdomain support, which it does not have")
	}
}
