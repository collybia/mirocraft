package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/collybia/mirocraft/internal/store"
)

// The files each proxy listens according to.
const (
	velocityConfig   = "velocity.toml"
	bungeeCordConfig = "config.yml"
)

// velocityBind matches the bind line in velocity.toml:
//
//	bind = "0.0.0.0:25577"
var velocityBind = regexp.MustCompile(`(?m)^\s*bind\s*=\s*".*"`)

// bungeeHost matches a listener's host line in BungeeCord's config.yml:
//
//   - host: 0.0.0.0:25577
var bungeeHost = regexp.MustCompile(`(?m)^(\s*-?\s*host:\s*)\S+`)

// applyProxyPort writes the panel's port into whichever config the proxy reads.
//
// The port is the panel's to own for a proxy exactly as it is for a server:
// the panel allocated it, published it in DNS and told the operator it. A
// proxy listening somewhere else means players connect to nothing.
//
// The file is only edited, never created: a proxy writes its own default
// config on first start, and one written here would be a guess at a format
// that changes between versions. So the first start listens on the proxy's own
// default and the second on the panel's port — noted in the log rather than
// papered over, because a port that changes on the second start is exactly the
// sort of thing that looks like a bug when it happens silently.
func (p *Provisioner) applyProxyPort(srv *store.Server, dir string) error {
	port := strconv.Itoa(srv.Port)

	for name, rewrite := range map[string]func(string, string) (string, bool){
		velocityConfig:   rewriteVelocityBind,
		bungeeCordConfig: rewriteBungeeHost,
	} {
		path := filepath.Join(dir, name)
		// A path built from the server's own directory and a fixed name.
		raw, err := os.ReadFile(path) // #nosec G304 -- a known file in the server's directory
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("reading %s of %s: %w", name, srv.ID, err)
		}

		updated, changed := rewrite(string(raw), port)
		if !changed {
			continue
		}
		if err := os.WriteFile(path, []byte(updated), 0o640); err != nil {
			return fmt.Errorf("writing %s of %s: %w", name, srv.ID, err)
		}
		p.log.Info("applied the panel's port to the proxy configuration",
			slog.String("server_id", srv.ID), slog.String("file", name),
			slog.Int("port", srv.Port))
		return nil
	}

	// Nothing to edit yet. Said at debug rather than warned: on a first start
	// this is the normal case, and warning on every normal case is how a log
	// stops being read.
	p.log.Debug("the proxy has not written its configuration yet; the port will be applied on the next start",
		slog.String("server_id", srv.ID))
	return nil
}

// rewriteVelocityBind replaces the address in velocity.toml, keeping whatever
// interface it was bound to.
func rewriteVelocityBind(body, port string) (string, bool) {
	match := velocityBind.FindString(body)
	if match == "" {
		return body, false
	}

	host := "0.0.0.0"
	if at := strings.Index(match, `"`); at >= 0 {
		value := strings.Trim(match[at:], `"`)
		if colon := strings.LastIndex(value, ":"); colon > 0 {
			host = value[:colon]
		}
	}

	replaced := fmt.Sprintf(`bind = "%s:%s"`, host, port)
	if match == replaced {
		return body, false
	}
	return velocityBind.ReplaceAllString(body, replaced), true
}

// rewriteBungeeHost replaces every listener's port in BungeeCord's config,
// keeping the interface each was bound to.
func rewriteBungeeHost(body, port string) (string, bool) {
	changed := false
	updated := bungeeHost.ReplaceAllStringFunc(body, func(line string) string {
		match := bungeeHost.FindStringSubmatch(line)
		if match == nil {
			return line
		}
		prefix := match[1]
		value := strings.TrimSpace(strings.TrimPrefix(line, prefix))

		host := "0.0.0.0"
		if colon := strings.LastIndex(value, ":"); colon > 0 {
			host = value[:colon]
		}
		replaced := prefix + host + ":" + port
		if replaced != line {
			changed = true
		}
		return replaced
	})
	return updated, changed
}
