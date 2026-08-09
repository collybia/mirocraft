package dns

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// PublicIPSources are asked what the outside world sees.
//
// Three, from unrelated operators, because the answer decides where players
// are pointed. A single source that is wrong — or that has been taken over —
// sends every server on the panel to an address the operator does not own,
// and nothing about the panel would look broken.
var PublicIPSources = []string{
	"https://api.ipify.org",
	"https://icanhazip.com",
	"https://ifconfig.co/ip",
}

// ipTimeout bounds one lookup. These services answer in milliseconds; waiting
// longer than this means one is down, and there are others.
const ipTimeout = 8 * time.Second

// PublicIP discovers the address the outside world sees.
//
// All sources are asked at once and the answer given by at least two is used.
// Where none agree the first success is taken and the disagreement is
// reported, so the caller can log it rather than silently pick a side.
func PublicIP(ctx context.Context, client *http.Client) (netip.Addr, error) {
	return publicIPFrom(ctx, client, PublicIPSources)
}

func publicIPFrom(ctx context.Context, client *http.Client, sources []string) (netip.Addr, error) {
	if client == nil {
		client = &http.Client{Timeout: ipTimeout}
	}
	if len(sources) == 0 {
		return netip.Addr{}, fmt.Errorf("dns: no public address sources configured")
	}

	type answer struct {
		addr netip.Addr
		err  error
	}

	answers := make([]answer, len(sources))
	var wg sync.WaitGroup

	for i, source := range sources {
		wg.Add(1)
		go func(i int, source string) {
			defer wg.Done()
			addr, err := fetchIP(ctx, client, source)
			answers[i] = answer{addr: addr, err: err}
		}(i, source)
	}
	wg.Wait()

	counts := map[netip.Addr]int{}
	var first netip.Addr
	var lastErr error

	for _, a := range answers {
		if a.err != nil {
			lastErr = a.err
			continue
		}
		counts[a.addr]++
		if !first.IsValid() {
			first = a.addr
		}
	}

	if len(counts) == 0 {
		return netip.Addr{}, fmt.Errorf("dns: no source could report the public address: %w", lastErr)
	}

	for addr, count := range counts {
		if count >= 2 {
			return addr, nil
		}
	}

	// One source answered, or several disagreed. Either is usable — the
	// record can be corrected on the next tick — but it is worth saying so.
	if len(counts) > 1 {
		return first, fmt.Errorf("dns: sources disagree about the public address (%s); using %s",
			formatCounts(counts), first)
	}
	return first, nil
}

func fetchIP(ctx context.Context, client *http.Client, source string) (netip.Addr, error) {
	// Defaulted here as well as in the caller: this is callable on its own —
	// checking one source at a time is how a broken one gets named — and a nil
	// client panics inside net/http rather than saying what is missing.
	if client == nil {
		client = &http.Client{Timeout: ipTimeout}
	}

	reqCtx, cancel := context.WithTimeout(ctx, ipTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, source, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	// Some of these serve a web page to a browser and plain text to anything
	// else; asking for text is what keeps the answer parseable.
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "mirocraft/dns")

	resp, err := client.Do(req)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%s: %w", source, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("%s: %s", source, resp.Status)
	}

	// Bounded: a source that started serving HTML must not be read into memory
	// in full before being rejected.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%s: %w", source, err)
	}

	addr, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%s answered %q, which is not an address",
			source, strings.TrimSpace(string(body)))
	}
	return addr.Unmap(), nil
}

func formatCounts(counts map[netip.Addr]int) string {
	parts := make([]string, 0, len(counts))
	for addr, count := range counts {
		parts = append(parts, fmt.Sprintf("%s×%d", addr, count))
	}
	return strings.Join(parts, ", ")
}
