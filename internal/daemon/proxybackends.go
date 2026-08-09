package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/collybia/mirocraft/internal/gamefiles"
	"github.com/collybia/mirocraft/internal/store"
)

// forwardingSecretFile is where Velocity keeps the secret its backends must
// know. Its own default name, so an operator who reads Velocity's docs finds
// what those docs describe.
const forwardingSecretFile = "forwarding.secret"

// velocityServers matches the [servers] table in velocity.toml, up to the next
// section. The table is rewritten wholesale: it is the panel's to own, and
// merging entry by entry would leave a server that was unlinked still listed.
var velocityServers = regexp.MustCompile(`(?ms)^\[servers\].*?(?:\n\[|\z)`)

// bungeeServers matches the servers block in BungeeCord's config.yml: the key
// and the indented lines under it.
//
// (?m) and deliberately not (?s): with the s flag the dot matches newlines, so
// the first indented line swallows the rest of the file and every key after
// the block disappears. A test caught exactly that.
var bungeeServers = regexp.MustCompile(`(?m)^servers:\n(?:[ \t]+[^\n]*\n?)*`)

// applyProxyBackends writes the panel's servers into a proxy's configuration.
//
// The panel knows every backend's port because it allocated them, so keeping
// this list by hand would be asking an operator to copy numbers the panel
// already has — and to remember to change them when a port moves.
func (p *Provisioner) applyProxyBackends(proxy *store.Server, dir string, backends []*store.Server) error {
	if len(backends) == 0 {
		// Nothing linked yet. The proxy's own list is left alone rather than
		// emptied: an operator may have written entries by hand, and wiping
		// them because the panel has nothing to say would be the panel
		// deciding it owns a file it has never written to.
		return nil
	}

	sort.SliceStable(backends, func(i, j int) bool { return backends[i].Name < backends[j].Name })

	velocity := filepath.Join(dir, velocityConfig)
	// A known file name under the server's own directory.
	if raw, err := os.ReadFile(velocity); err == nil { // #nosec G304 -- a known file in the server's directory
		updated, changed := rewriteVelocityServers(string(raw), backends)
		if changed {
			// A fixed file name under the server's own directory.
			if err := os.WriteFile(velocity, []byte(updated), 0o640); err != nil { // #nosec G703 -- a known name in the server's directory
				return fmt.Errorf("writing the backend list of %s: %w", proxy.ID, err)
			}
			p.log.Info("wrote the proxy's backend list",
				slog.String("server_id", proxy.ID), slog.Int("backends", len(backends)))
		}
		return nil
	}

	bungee := filepath.Join(dir, bungeeCordConfig)
	if raw, err := os.ReadFile(bungee); err == nil { // #nosec G304 -- a known file in the server's directory
		updated, changed := rewriteBungeeServers(string(raw), backends)
		if changed {
			if err := os.WriteFile(bungee, []byte(updated), 0o640); err != nil { // #nosec G703 -- a known name in the server's directory
				return fmt.Errorf("writing the backend list of %s: %w", proxy.ID, err)
			}
			p.log.Info("wrote the proxy's backend list",
				slog.String("server_id", proxy.ID), slog.Int("backends", len(backends)))
		}
		return nil
	}

	p.log.Debug("the proxy has not written its configuration yet; backends will be written on the next start",
		slog.String("server_id", proxy.ID))
	return nil
}

// rewriteVelocityServers replaces the [servers] table.
func rewriteVelocityServers(body string, backends []*store.Server) (string, bool) {
	var table strings.Builder
	table.WriteString("[servers]\n")
	for _, backend := range backends {
		fmt.Fprintf(&table, "%s = \"127.0.0.1:%d\"\n", velocityKey(backend.Name), backend.Port)
	}
	// try holds the order players are sent in when they connect. The first
	// backend by name, because a proxy with no try list drops everyone who
	// connects with "no available servers".
	fmt.Fprintf(&table, "try = [\"%s\"]\n", velocityKey(backends[0].Name))

	match := velocityServers.FindStringIndex(body)
	if match == nil {
		// No table at all: appended, since a Velocity config without one
		// accepts nobody.
		return strings.TrimRight(body, "\n") + "\n\n" + table.String(), true
	}

	// The match may have swallowed the opening bracket of the next section.
	end := match[1]
	tail := ""
	if strings.HasSuffix(body[match[0]:end], "\n[") {
		end--
		tail = "["
	}

	replaced := body[:match[0]] + table.String() + tail + body[end:]
	if replaced == body {
		return body, false
	}
	return replaced, true
}

// velocityKey turns a server name into a key Velocity accepts: it is a TOML
// bare key, so anything but letters, digits, dashes and underscores has to go.
func velocityKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	key := strings.Trim(b.String(), "-")
	if key == "" {
		return "server"
	}
	return key
}

// rewriteBungeeServers replaces the servers block in BungeeCord's config.
func rewriteBungeeServers(body string, backends []*store.Server) (string, bool) {
	var block strings.Builder
	block.WriteString("servers:\n")
	for _, backend := range backends {
		fmt.Fprintf(&block, "  %s:\n", velocityKey(backend.Name))
		fmt.Fprintf(&block, "    address: 127.0.0.1:%d\n", backend.Port)
		fmt.Fprintf(&block, "    restricted: false\n")
		fmt.Fprintf(&block, "    motd: '%s'\n", backend.Name)
	}

	if !bungeeServers.MatchString(body) {
		return strings.TrimRight(body, "\n") + "\n" + block.String(), true
	}
	replaced := bungeeServers.ReplaceAllString(body, block.String())
	if replaced == body {
		return body, false
	}
	return replaced, true
}

// ensureForwardingSecret makes sure a Velocity proxy has one and returns it.
//
// Velocity's modern forwarding is what carries a player's identity from the
// proxy to the backend. Without it a backend in offline mode accepts anyone
// under any name, which is the difference between a proxy setup and an open
// server.
func (p *Provisioner) ensureForwardingSecret(dir string) (string, error) {
	path := filepath.Join(dir, forwardingSecretFile)

	// The proxy writes one itself on first start; whatever is there wins, so
	// the panel never overwrites a secret the proxy and its backends already
	// agree on.
	if raw, err := os.ReadFile(path); err == nil { // #nosec G304 -- a known file in the server's directory
		if secret := strings.TrimSpace(string(raw)); secret != "" {
			return secret, nil
		}
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a forwarding secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)

	if err := os.WriteFile(path, []byte(secret), 0o640); err != nil {
		return "", fmt.Errorf("writing the forwarding secret: %w", err)
	}
	return secret, nil
}

// applyProxyLinks writes a proxy's backend list and makes sure it has a
// forwarding secret.
//
// Reads the backends from the store rather than taking them as an argument:
// this runs before every start, and the list an operator changed while the
// proxy was down has to reach it.
func (p *Provisioner) applyProxyLinks(proxy *store.Server, dir string) error {
	if p.Servers == nil {
		return nil
	}

	backends, err := p.Servers.Backends(context.Background(), proxy.ID)
	if err != nil {
		return fmt.Errorf("reading the servers behind %s: %w", proxy.ID, err)
	}
	if err := p.applyProxyBackends(proxy, dir, backends); err != nil {
		return err
	}

	// Only Velocity has one; BungeeCord forwards by IP and has nothing to
	// share.
	if proxy.Core == "velocity" {
		if _, err := p.ensureForwardingSecret(dir); err != nil {
			return err
		}
	}
	return nil
}

// applyBackendSettings prepares a server to sit behind a proxy.
//
// Two settings, both of which produce a server that looks broken when they are
// missing: online-mode has to be off, because the proxy has already
// authenticated the player and a second attempt fails; and the proxy's
// forwarding has to be enabled, or the backend refuses connections it cannot
// attribute to anyone.
func (p *Provisioner) applyBackendSettings(srv *store.Server, dir string) error {
	if p.Servers == nil {
		return nil
	}

	proxy, err := p.Servers.GetByID(context.Background(), srv.ProxyID)
	if err != nil {
		// The proxy was deleted; the link is stale. Left as it is rather than
		// failing the start: a server whose proxy is gone should still run.
		p.log.Warn("the proxy this server is linked to no longer exists",
			slog.String("server_id", srv.ID), slog.String("proxy_id", srv.ProxyID))
		return nil //nolint:nilerr // a server whose proxy is gone should still start
	}

	properties, err := gamefiles.LoadProperties(dir)
	if err != nil {
		return fmt.Errorf("reading server.properties of %s: %w", srv.ID, err)
	}
	if current, ok := properties.Get("online-mode"); !ok || current != "false" {
		properties.Set("online-mode", "false")
		if err := properties.Save(dir); err != nil {
			return fmt.Errorf("writing server.properties of %s: %w", srv.ID, err)
		}
		p.log.Info("turned online-mode off: this server is behind a proxy",
			slog.String("server_id", srv.ID), slog.String("proxy_id", proxy.ID))
	}
	return nil
}
