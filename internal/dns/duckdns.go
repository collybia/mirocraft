package dns

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// DuckDNSBaseURL is the DuckDNS update endpoint.
const DuckDNSBaseURL = "https://www.duckdns.org/update"

// DuckDNSSuffix is the only zone DuckDNS serves.
const DuckDNSSuffix = ".duckdns.org"

// DuckDNS publishes records through duckdns.org.
//
// The simplest thing that works, and the most limited: one name, an address
// and a TXT record, no subdomains and no SRV. That last one is not a detail —
// a Minecraft server on any port but 25565 needs SRV to be reachable by name
// alone, so a DuckDNS install either keeps the default port or asks players
// to type it.
//
// Kept anyway because it is the fastest free name to obtain: a GitHub login
// and a token, no email confirmation, no account to manage.
type DuckDNS struct {
	// name is the label without the suffix: "myserver" for myserver.duckdns.org.
	name  string
	token string

	BaseURL string
	HTTP    *http.Client
}

var _ Provider = (*DuckDNS)(nil)

// NewDuckDNS builds the provider.
func NewDuckDNS(cfg Config, httpClient *http.Client) (*DuckDNS, error) {
	zone := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.Zone), "."))
	if zone == "" {
		return nil, fmt.Errorf("dns: DuckDNS needs a name")
	}
	// Both forms are accepted because both are what an operator has in front
	// of them: the site shows "myserver", the browser shows the full name.
	name := strings.TrimSuffix(zone, DuckDNSSuffix)
	if name == "" || strings.Contains(name, ".") {
		return nil, fmt.Errorf("dns: %q is not a DuckDNS name; expected myname or myname%s",
			cfg.Zone, DuckDNSSuffix)
	}
	if err := ValidateSub(name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("dns: DuckDNS needs a token")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}

	return &DuckDNS{name: name, token: cfg.Token, BaseURL: DuckDNSBaseURL, HTTP: httpClient}, nil
}

// ID returns the identifier this provider is configured under.
func (d *DuckDNS) ID() string { return "duckdns" }

// Name returns the name shown in the panel.
func (d *DuckDNS) Name() string { return "DuckDNS" }

// Zone returns the domain records are created under.
func (d *DuckDNS) Zone() string { return d.name + DuckDNSSuffix }

// Capabilities reports what this provider can and cannot do.
func (d *DuckDNS) Capabilities() Capabilities {
	// No TTL of its own to report: DuckDNS does not let one be chosen.
	//
	// DNS01 is true despite there being no general TXT support, and that is
	// not a contradiction: DuckDNS publishes exactly one TXT record, served at
	// _acme-challenge.<name>.duckdns.org, put there specifically so its users
	// can obtain certificates.
	return Capabilities{SRV: false, DNS01: true, Subdomains: false}
}

// EnsureTXT publishes the one TXT record DuckDNS serves.
//
// sub must be the ACME challenge label: it is the only name DuckDNS will
// answer a TXT query for, and pretending otherwise would leave a validator
// looking at nothing.
func (d *DuckDNS) EnsureTXT(ctx context.Context, sub string, values []string) error {
	if sub != ACMEChallengeLabel {
		return fmt.Errorf("%w: DuckDNS serves a TXT record only at %s.%s",
			ErrUnsupported, ACMEChallengeLabel, d.Zone())
	}
	if len(values) == 0 {
		return d.DeleteTXT(ctx, sub)
	}
	// One value only: the parameter holds a single token, so a certificate
	// needing two — a name and its wildcard — cannot be issued here. Said
	// plainly rather than publishing the first and letting the second fail.
	if len(values) > 1 {
		return fmt.Errorf("%w: DuckDNS holds one TXT value, but %d were needed "+
			"(a wildcard certificate needs two)", ErrUnsupported, len(values))
	}

	return d.call(ctx, url.Values{
		"domains": {d.name}, "token": {d.token},
		"txt": {values[0]}, "verbose": {"true"},
	})
}

// DeleteTXT clears the TXT record.
func (d *DuckDNS) DeleteTXT(ctx context.Context, sub string) error {
	if sub != ACMEChallengeLabel {
		return fmt.Errorf("%w: DuckDNS serves a TXT record only at %s.%s",
			ErrUnsupported, ACMEChallengeLabel, d.Zone())
	}
	// clear=true wipes the TXT alongside the address, so the record is
	// overwritten with an empty value instead — the address must survive.
	return d.call(ctx, url.Values{
		"domains": {d.name}, "token": {d.token}, "txt": {""}, "verbose": {"true"},
	})
}

// EnsureAddress publishes the address for the registered name.
//
// sub must be empty: DuckDNS serves exactly one name and nothing beneath it.
// Refused rather than ignored, because silently writing mc.example to the
// apex would give an operator a record they did not ask for and a name that
// does not resolve.
func (d *DuckDNS) EnsureAddress(ctx context.Context, sub string, ip netip.Addr) error {
	if sub != "" {
		return fmt.Errorf("%w: DuckDNS has no subdomains, only %s", ErrUnsupported, d.Zone())
	}
	if !ip.IsValid() {
		return fmt.Errorf("dns: %q is not a valid address", ip)
	}

	query := url.Values{"domains": {d.name}, "token": {d.token}, "verbose": {"true"}}
	// The two families go in separate parameters; sending an address in the
	// wrong one is accepted and then does nothing.
	if ip.Is4() {
		query.Set("ip", ip.Unmap().String())
	} else {
		query.Set("ipv6", ip.String())
	}

	return d.call(ctx, query)
}

// EnsureSRV is not possible here.
func (d *DuckDNS) EnsureSRV(context.Context, string, string, int) error {
	return fmt.Errorf("%w: DuckDNS cannot publish SRV records, so a server on a "+
		"non-standard port will not be found by name alone", ErrUnsupported)
}

// Cleanup clears the address.
//
// DuckDNS has no delete; clearing is done by asking for an update with the
// address parameter set to a literal "clear".
func (d *DuckDNS) Cleanup(ctx context.Context, sub string) error {
	if sub != "" {
		return fmt.Errorf("%w: DuckDNS has no subdomains", ErrUnsupported)
	}
	return d.call(ctx, url.Values{
		"domains": {d.name}, "token": {d.token}, "clear": {"true"}, "verbose": {"true"},
	})
}

// call performs an update request.
//
// DuckDNS answers 200 whatever happens and puts the outcome in the body: "OK"
// or "KO". Trusting the status code would report every failure — a wrong
// token above all — as success, and the operator would be left wondering why
// the name never resolves.
func (d *DuckDNS) call(ctx context.Context, query url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+"?"+query.Encode(), nil)
	if err != nil {
		return fmt.Errorf("dns: building the request: %w", err)
	}

	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("dns: DuckDNS unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return fmt.Errorf("dns: reading the DuckDNS response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("dns: DuckDNS: %s", resp.Status)
	}

	text := strings.TrimSpace(string(body))
	first, _, _ := strings.Cut(text, "\n")
	switch strings.TrimSpace(first) {
	case "OK":
		return nil
	case "KO":
		// The API says nothing more than "KO", so the likely cause is named
		// here rather than passing on two useless letters.
		return fmt.Errorf("%w: DuckDNS answered KO — usually a wrong token or a "+
			"name that is not registered to it", ErrAuth)
	default:
		return fmt.Errorf("dns: DuckDNS answered %q", text)
	}
}
