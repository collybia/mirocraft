package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CloudflareBaseURL is the Cloudflare API.
const CloudflareBaseURL = "https://api.cloudflare.com/client/v4"

// CloudflareMinTTL is the shortest TTL Cloudflare accepts on a free plan.
const CloudflareMinTTL = 60

// Cloudflare publishes records in a zone the operator already owns.
//
// The path for someone who has a domain rather than wanting one. It speaks
// every record type the panel needs, and its token model is narrow enough to
// hand over safely: a token scoped to Zone:DNS:Edit on one zone can do nothing
// else to the account.
type Cloudflare struct {
	zone  string
	token string
	ttl   int

	BaseURL string
	HTTP    *http.Client

	// zoneID is looked up once and remembered: it never changes for a zone,
	// and looking it up on every update would double the requests for nothing.
	mu     sync.Mutex
	zoneID string
}

var _ Provider = (*Cloudflare)(nil)

// NewCloudflare builds the provider.
func NewCloudflare(cfg Config, httpClient *http.Client) (*Cloudflare, error) {
	zone := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.Zone), "."))
	if zone == "" {
		return nil, fmt.Errorf("dns: Cloudflare needs a zone, for example example.com")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("dns: Cloudflare needs an API token with Zone:DNS:Edit")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}

	return &Cloudflare{
		zone: zone, token: cfg.Token,
		ttl:     effectiveTTL(cfg.TTL, CloudflareMinTTL),
		BaseURL: CloudflareBaseURL, HTTP: httpClient,
	}, nil
}

func (c *Cloudflare) ID() string   { return "cloudflare" }
func (c *Cloudflare) Name() string { return "Cloudflare" }
func (c *Cloudflare) Zone() string { return c.zone }

func (c *Cloudflare) Capabilities() Capabilities {
	return Capabilities{SRV: true, Subdomains: true, MinTTL: CloudflareMinTTL}
}

// cfEnvelope is the shape every Cloudflare response takes.
type cfEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cfError       `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	// Proxied must be false for anything but HTTP: Cloudflare's proxy does not
	// carry the Minecraft protocol, and a proxied A record points players at
	// Cloudflare's edge, where nothing is listening for them.
	Proxied bool `json:"proxied"`
}

// EnsureAddress publishes an A or AAAA record.
func (c *Cloudflare) EnsureAddress(ctx context.Context, sub string, ip netip.Addr) error {
	if err := ValidateSub(sub); err != nil {
		return err
	}
	if !ip.IsValid() {
		return fmt.Errorf("dns: %q is not a valid address", ip)
	}

	return c.upsert(ctx, cfRecord{
		Type: RecordType(ip), Name: FQDN(sub, c.zone),
		Content: ip.Unmap().String(), TTL: c.ttl, Proxied: false,
	})
}

// EnsureSRV publishes _minecraft._tcp.<sub>.
//
// Cloudflare accepts SRV either as a structured document or as the plain
// record text in content. The text form is used because it is the same string
// every other provider takes, so the value cannot drift between them.
func (c *Cloudflare) EnsureSRV(ctx context.Context, sub, target string, port int) error {
	if err := ValidateSub(sub); err != nil {
		return err
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("dns: %d is not a valid port", port)
	}

	name := MinecraftSRVPrefix + "." + FQDN(sub, c.zone)
	return c.upsert(ctx, cfRecord{
		Type: "SRV", Name: name, Content: SRVValue(target, port), TTL: c.ttl,
	})
}

// Cleanup deletes the records this panel created for sub.
func (c *Cloudflare) Cleanup(ctx context.Context, sub string) error {
	if err := ValidateSub(sub); err != nil {
		return err
	}

	name := FQDN(sub, c.zone)
	for _, target := range []struct{ recordType, recordName string }{
		{"A", name},
		{"AAAA", name},
		{"SRV", MinecraftSRVPrefix + "." + name},
	} {
		existing, err := c.find(ctx, target.recordType, target.recordName)
		if err != nil {
			return err
		}
		for _, record := range existing {
			if err := c.delete(ctx, record.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// upsert creates a record or updates the one already there.
//
// Cloudflare has no "create or replace", so the existing record is found
// first. Creating blindly would leave two A records for one name, and a
// resolver handing out both means half the players reach an address that
// stopped being right months ago.
func (c *Cloudflare) upsert(ctx context.Context, record cfRecord) error {
	existing, err := c.find(ctx, record.Type, record.Name)
	if err != nil {
		return err
	}

	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("dns: encoding the record: %w", err)
	}

	zoneID, err := c.resolveZoneID(ctx)
	if err != nil {
		return err
	}

	if len(existing) == 0 {
		return c.call(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, nil)
	}

	// Extra copies from an earlier run, or from someone editing by hand, are
	// removed rather than left: they would be served alongside the right one.
	for _, stale := range existing[1:] {
		if err := c.delete(ctx, stale.ID); err != nil {
			return err
		}
	}
	return c.call(ctx, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+existing[0].ID, body, nil)
}

// find returns matching records.
func (c *Cloudflare) find(ctx context.Context, recordType, name string) ([]cfRecord, error) {
	zoneID, err := c.resolveZoneID(ctx)
	if err != nil {
		return nil, err
	}

	query := url.Values{"type": {recordType}, "name": {name}}
	var records []cfRecord
	if err := c.call(ctx, http.MethodGet,
		"/zones/"+zoneID+"/dns_records?"+query.Encode(), nil, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func (c *Cloudflare) delete(ctx context.Context, id string) error {
	zoneID, err := c.resolveZoneID(ctx)
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+id, nil, nil)
}

// resolveZoneID looks the zone up by name, once.
func (c *Cloudflare) resolveZoneID(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.zoneID
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	query := url.Values{"name": {c.zone}}
	if err := c.call(ctx, http.MethodGet, "/zones?"+query.Encode(), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("%w: the token cannot see the zone %s — check that it is "+
			"scoped to this zone and that the domain is on this account", ErrNotFound, c.zone)
	}

	c.mu.Lock()
	c.zoneID = zones[0].ID
	c.mu.Unlock()
	return zones[0].ID, nil
}

// call performs a request and unwraps Cloudflare's envelope.
func (c *Cloudflare) call(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("dns: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("dns: Cloudflare unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("dns: reading the Cloudflare response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: Cloudflare refused the token; it needs Zone:DNS:Edit on %s",
			ErrAuth, c.zone)
	}

	var envelope cfEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("dns: Cloudflare answered %s with something that is not JSON: %s",
			resp.Status, strings.TrimSpace(string(raw)))
	}

	// Cloudflare can answer 200 with success:false, so the envelope is what
	// decides — the status alone would report a refusal as a success.
	if !envelope.Success {
		return fmt.Errorf("dns: Cloudflare: %s", formatCFErrors(envelope.Errors, resp.Status))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("dns: decoding the Cloudflare result: %w", err)
	}
	return nil
}

func formatCFErrors(errs []cfError, status string) string {
	if len(errs) == 0 {
		return status
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}
