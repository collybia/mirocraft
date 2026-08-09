package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// DeSECBaseURL is the deSEC API.
const DeSECBaseURL = "https://desec.io/api/v1"

// DeSECMinTTL is the shortest TTL deSEC accepts.
//
// Their free tier refuses anything lower, and asking for 300 gets the whole
// request rejected rather than quietly rounded — so the floor is applied here
// instead of being discovered from a 400.
const DeSECMinTTL = 3600

// DeSEC publishes records through desec.io.
//
// The default for an operator with no domain: it hands out a free name under
// dedyn.io and, unlike DuckDNS, speaks the full record set — including SRV,
// which is what lets a server on a non-standard port be found by name alone.
type DeSEC struct {
	domain string
	token  string
	ttl    int

	BaseURL string
	HTTP    *http.Client
}

var _ Provider = (*DeSEC)(nil)

// NewDeSEC builds the provider.
func NewDeSEC(cfg Config, httpClient *http.Client) (*DeSEC, error) {
	if strings.TrimSpace(cfg.Zone) == "" {
		return nil, fmt.Errorf("dns: deSEC needs a domain")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("dns: deSEC needs an API token")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}

	return &DeSEC{
		domain:  strings.TrimSuffix(strings.ToLower(cfg.Zone), "."),
		token:   cfg.Token,
		ttl:     effectiveTTL(cfg.TTL, DeSECMinTTL),
		BaseURL: DeSECBaseURL,
		HTTP:    httpClient,
	}, nil
}

// ID returns the identifier this provider is configured under.
func (d *DeSEC) ID() string { return "desec" }

// Name returns the name shown in the panel.
func (d *DeSEC) Name() string { return "deSEC" }

// Zone returns the domain records are created under.
func (d *DeSEC) Zone() string { return d.domain }

// Capabilities reports what this provider can and cannot do.
func (d *DeSEC) Capabilities() Capabilities {
	return Capabilities{SRV: true, DNS01: true, Subdomains: true, MinTTL: DeSECMinTTL}
}

// EnsureTXT publishes a TXT record.
//
// The values are quoted here because deSEC stores record content in its
// presentation form: an unquoted token is rejected outright, which is the kind
// of thing that costs an afternoon the first time.
func (d *DeSEC) EnsureTXT(ctx context.Context, sub string, values []string) error {
	if err := ValidateTXTSub(sub); err != nil {
		return err
	}

	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return d.putRRSets(ctx, []rrset{{Subname: sub, Type: "TXT", TTL: d.ttl, Records: quoted}})
}

// DeleteTXT removes a TXT record by writing it empty.
func (d *DeSEC) DeleteTXT(ctx context.Context, sub string) error {
	if err := ValidateTXTSub(sub); err != nil {
		return err
	}
	return d.putRRSets(ctx, []rrset{{Subname: sub, Type: "TXT", TTL: d.ttl, Records: []string{}}})
}

// rrset is deSEC's record-set document.
type rrset struct {
	Subname string   `json:"subname"`
	Type    string   `json:"type"`
	TTL     int      `json:"ttl"`
	Records []string `json:"records"`
}

// EnsureAddress publishes an A or AAAA record.
func (d *DeSEC) EnsureAddress(ctx context.Context, sub string, ip netip.Addr) error {
	if err := ValidateSub(sub); err != nil {
		return err
	}
	if !ip.IsValid() {
		return fmt.Errorf("dns: %q is not a valid address", ip)
	}

	return d.putRRSets(ctx, []rrset{{
		Subname: sub, Type: RecordType(ip), TTL: d.ttl,
		Records: []string{ip.Unmap().String()},
	}})
}

// EnsureSRV publishes _minecraft._tcp.<sub>.
func (d *DeSEC) EnsureSRV(ctx context.Context, sub, target string, port int) error {
	if err := ValidateSub(sub); err != nil {
		return err
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("dns: %d is not a valid port", port)
	}

	name := MinecraftSRVPrefix
	if sub != "" {
		name = MinecraftSRVPrefix + "." + sub
	}
	return d.putRRSets(ctx, []rrset{{
		Subname: name, Type: "SRV", TTL: d.ttl,
		Records: []string{SRVValue(target, port)},
	}})
}

// Cleanup removes the records this panel published for sub.
//
// deSEC deletes a record set by writing it empty, which is also how a set that
// never existed is handled — so removing something twice is not an error.
func (d *DeSEC) Cleanup(ctx context.Context, sub string) error {
	if err := ValidateSub(sub); err != nil {
		return err
	}

	srvName := MinecraftSRVPrefix
	if sub != "" {
		srvName = MinecraftSRVPrefix + "." + sub
	}

	return d.putRRSets(ctx, []rrset{
		{Subname: sub, Type: "A", TTL: d.ttl, Records: []string{}},
		{Subname: sub, Type: "AAAA", TTL: d.ttl, Records: []string{}},
		{Subname: srvName, Type: "SRV", TTL: d.ttl, Records: []string{}},
	})
}

// putRRSets writes record sets, creating or replacing them.
//
// PUT on the collection rather than POST: it is idempotent, which is what a
// dynamic-DNS updater running every few minutes needs. POST would fail the
// second time with "already exists" and turn a working updater into a log
// full of errors.
func (d *DeSEC) putRRSets(ctx context.Context, sets []rrset) error {
	body, err := json.Marshal(sets)
	if err != nil {
		return fmt.Errorf("dns: encoding the request: %w", err)
	}

	url := d.BaseURL + "/domains/" + d.domain + "/rrsets/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("dns: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+d.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("dns: deSEC unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: deSEC refused the token", ErrAuth)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: deSEC does not know the domain %s", ErrNotFound, d.domain)
	case resp.StatusCode >= 400:
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		// The body carries the field-level reason, and dropping it in favour
		// of the status turns "ttl must be at least 3600" into "400".
		return fmt.Errorf("dns: deSEC: %s: %s", resp.Status, strings.TrimSpace(string(preview)))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
