package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/collybia/mirocraft/internal/core"
	"github.com/collybia/mirocraft/internal/store"
)

// geyserConfig is where Geyser keeps its settings, relative to the server
// directory. The path is Geyser's own convention on every platform it runs on.
const geyserConfig = "plugins/Geyser-Spigot/config.yml"

// The lines in Geyser's config that the panel owns.
//
// Rewritten rather than generated: the file is long, full of comments that
// explain each option, and an operator will edit it. Writing it from scratch
// would throw all of that away every start.
var (
	geyserPort     = regexp.MustCompile(`^(\s*port:\s*)\d+`)
	geyserAuthType = regexp.MustCompile(`^(\s*auth-type:\s*)\S+`)
)

// lineBreak splits a file without caring which line endings it uses.
var lineBreak = regexp.MustCompile("\r?\n")

// applyCrossplay installs Geyser and Floodgate and points them at this server.
//
// Both, always: Geyser alone translates the protocol and then the server
// rejects the player for having no Mojang session, which reads as "crossplay
// does not work" rather than as a missing second plugin.
func (p *Provisioner) applyCrossplay(ctx context.Context, provider core.Provider, srv *store.Server, dir string) error {
	if !srv.Crossplay {
		return nil
	}
	if p.Crossplay == nil {
		return fmt.Errorf("crossplay needs the geyser downloads, which this daemon was not given")
	}

	content := provider.Content()
	if !content.Accepts() {
		return fmt.Errorf("%s takes no plugins, so crossplay cannot be installed on it", provider.ID())
	}

	floodgate := false
	for _, project := range []string{core.GeyserProject, core.FloodgateProject} {
		platform, ok := core.PlatformFor(project, content.Loader)
		if !ok {
			// Floodgate publishes for fewer platforms than Geyser. A Fabric
			// server gets Geyser and no Floodgate, which is a working setup
			// for online-mode servers and worth saying rather than failing.
			p.log.Warn("no crossplay build for this core",
				slog.String("project", project), slog.String("loader", content.Loader))
			continue
		}

		build, err := p.Crossplay.Resolve(ctx, project, platform)
		if err != nil {
			return fmt.Errorf("resolving %s for %s: %w", project, content.Loader, err)
		}

		target := filepath.Join(dir, content.Dir, build.FileName)
		if err := p.ensurePlugin(ctx, build, target); err != nil {
			return err
		}
		if project == core.FloodgateProject {
			floodgate = true
		}
	}

	return p.applyGeyserConfig(srv, dir, floodgate)
}

// ensurePlugin puts an add-on jar in place, downloading it if the cache does
// not already hold it.
func (p *Provisioner) ensurePlugin(ctx context.Context, build *core.Build, target string) error {
	cached, err := p.Downloader.Fetch(ctx, build)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", build.Core, err)
	}

	source, err := os.Stat(cached)
	if err != nil {
		return fmt.Errorf("reading the cached %s: %w", build.Core, err)
	}
	// Already there and the same size: this runs before every start, and
	// copying forty megabytes each time would add seconds to all of them.
	if existing, err := os.Stat(target); err == nil && existing.Size() == source.Size() {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating the plugin directory: %w", err)
	}
	if err := copyFile(cached, target); err != nil {
		return fmt.Errorf("installing %s: %w", build.Core, err)
	}

	p.log.Info("installed a crossplay add-on",
		slog.String("project", build.Core), slog.String("version", build.Version))
	return nil
}

// applyGeyserConfig points Geyser at this server and at its Bedrock port.
//
// Only after Geyser has run once: it writes the file itself on first start,
// and one written here would be this panel's guess at a format that changes
// between versions. So the first start listens on Geyser's default and the
// second on the panel's port — logged rather than hidden, because a port that
// changes on the second start looks like a fault when it happens quietly.
func (p *Provisioner) applyGeyserConfig(srv *store.Server, dir string, floodgate bool) error {
	if srv.BedrockPort <= 0 {
		return nil
	}

	path := filepath.Join(dir, filepath.FromSlash(geyserConfig))
	// A fixed path under the server's own directory.
	raw, err := os.ReadFile(path) // #nosec G304 -- a known file in the server's directory
	if err != nil {
		if os.IsNotExist(err) {
			p.log.Debug("geyser has not written its configuration yet; the port will be applied on the next start",
				slog.String("server_id", srv.ID))
			return nil
		}
		return fmt.Errorf("reading the geyser configuration of %s: %w", srv.ID, err)
	}

	updated, changed := rewriteGeyserConfig(string(raw), srv.BedrockPort, floodgate)
	if !changed {
		return nil
	}
	if err := os.WriteFile(path, []byte(updated), 0o640); err != nil { // #nosec G703 -- a known name in the server's directory
		return fmt.Errorf("writing the geyser configuration of %s: %w", srv.ID, err)
	}

	p.log.Info("configured geyser",
		slog.String("server_id", srv.ID),
		slog.Int("bedrock_port", srv.BedrockPort), slog.Bool("floodgate", floodgate))
	return nil
}

// rewriteGeyserConfig sets the port Geyser listens on and how it authenticates.
//
// Two sections matter and they are not the ones the documentation photographs
// suggest: in Geyser 2.11 the listener is under "bedrock:" and the connection
// to this server under "java:". An earlier version of this looked for
// "remote:", which does not exist in this release — read off the file Geyser
// actually wrote rather than from memory.
//
// auth-type is the one that decides whether crossplay works at all. Left at
// "online", Geyser asks a Bedrock player for a Java account, which is exactly
// what Floodgate was installed to avoid; the player sees a login prompt and
// the operator sees a working server.
func rewriteGeyserConfig(body string, bedrockPort int, floodgate bool) (string, bool) {
	lines := lineBreak.Split(body, -1)
	section := ""
	changed := false

	for i, line := range lines {
		trimmed := strings.TrimRight(line, " 	")
		switch {
		case trimmed == "bedrock:" || trimmed == "java:":
			section = trimmed
			continue
		case len(trimmed) > 0 && trimmed[0] != ' ' && trimmed[0] != '	' && trimmed[0] != '#':
			// A new top-level key ends whichever section was open.
			section = ""
		}

		switch section {
		case "bedrock:":
			if replaced := geyserPort.ReplaceAllString(line, "${1}"+strconv.Itoa(bedrockPort)); replaced != line {
				lines[i] = replaced
				changed = true
			}
		case "java:":
			if !floodgate {
				continue
			}
			if replaced := geyserAuthType.ReplaceAllString(line, "${1}floodgate"); replaced != line {
				lines[i] = replaced
				changed = true
			}
		}
	}

	if !changed {
		return body, false
	}
	return strings.Join(lines, "\n"), true
}
