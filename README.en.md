# Mirocraft

[![CI](https://github.com/collybia/mirocraft/actions/workflows/ci.yml/badge.svg)](https://github.com/collybia/mirocraft/actions/workflows/ci.yml)
[![License: AGPL v3](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)

**A Minecraft server control panel you host yourself.** Like Aternos, except the server
is yours: no queue to start it, no ads, no "your server went to sleep because nobody was
playing".

One binary is the daemon, the REST API and the web panel at once. No nginx, no MySQL, no
Redis, no separate agent on every machine.

[Русский](README.md) · [Documentation](https://collybia.github.io/mirocraft/)

---

## Install

**Ubuntu / Debian:**

```bash
curl -fsSL https://raw.githubusercontent.com/collybia/mirocraft/master/installer/install.sh | sudo bash
```

**Windows Server (PowerShell as administrator):**

```powershell
irm https://raw.githubusercontent.com/collybia/mirocraft/master/installer/install.ps1 | iex
```

The script asks how the panel should be reachable — a free subdomain, your own domain, or
just an address — and does the rest itself: user, service, firewall, certificate. It
prints the administrator's login and password at the end; they are also written to
`/var/lib/mirocraft/initial-admin.txt` (on Windows, in the service's data directory).

Running the same script again is the upgrade. The binary and the service are reinstalled;
the configuration, the database and every world are left alone. The upgrade path and the
install path are the same code — otherwise the upgrade path becomes the one nobody tests.

---

## What it looks like

| | |
|---|---|
| ![Server list](docs/screenshots/servers.png) | ![Server console](docs/screenshots/console.png) |
| Servers: core, port, memory, status | A WebSocket console, with memory, CPU, uptime, players |
| ![Modpacks](docs/screenshots/modpacks.png) | ![Themes](docs/screenshots/themes.png) |
| A modpack in one click, loader included | Five built-in themes and an editor for your own |

---

## What it does

**Servers**

- 19 cores: Vanilla, Paper, Purpur, Pufferfish, Folia, Fabric, Quilt, Forge, NeoForge,
  Sponge, Mohist, Arclight, Spigot (compiled through BuildTools, cached), the Velocity /
  BungeeCord / Waterfall proxies, and Bedrock: BDS, PowerNukkitX, PocketMine-MP.
- Versions come from each project's own API rather than from a list somebody once typed
  into the code.
- The panel installs the Java and PHP runtimes it needs. Nothing to install on the host.
- Docker when it is there, native processes when it is not. Windows Server through job
  objects rather than best-effort.

**One click**

- **Modpacks** from Modrinth: the panel downloads the pack, installs the loader and the
  Minecraft version it needs, and lays out the mods and configs.
- **Plugins and mods** — Modrinth search inside the panel, with dependencies and checksum
  verification.
- **Crossplay** — one switch installs Geyser and Floodgate, and Bedrock players join a
  Java server without a Java account.

**Around the server**

- WebSocket console, a file manager with an editor, and `server.properties` explained in
  plain language.
- Backups: on demand and on a schedule, with restore and download.
- Players: who is online, kick, ban, whitelist, operators.
- Scheduler: cron jobs with chains of actions (command → backup → restart).
- DNS and TLS: a free subdomain (deSEC / DuckDNS) or your own domain through Cloudflare,
  SRV records for Java, and a Let's Encrypt certificate that renews itself.

**Not only the web**

- **Discord and Telegram bots** out of the box: `/servers`, `/start`, `/stop`, `/status`,
  `/cmd`, `/console`. The token goes into the panel; there is no second process to run.
- **A REST API** that is not an accessory to the panel — it is what the panel itself
  uses. OpenAPI spec and Swagger UI at `/api/docs`.

---

## How it differs

|  | Mirocraft | Aternos | Pterodactyl |
|---|---|---|---|
| Runs on | your server | theirs | your server |
| Start queue, sleeping | no | yes | no |
| What you install | one binary | nothing | PHP, nginx, MySQL, Redis, Wings |
| Docker | optional | — | required |
| Windows Server | supported | — | no |
| Modpacks | one click | one click | by hand |
| Crossplay (Geyser) | a switch | by hand | by hand |
| Discord/Telegram bots | built in | no | third-party |
| Domain and TLS | the installer sets it up | their subdomain | you set it up |
| Hardware limits | yours | their plans | yours |

Pterodactyl is a mature panel built for hosting providers, and if you already run the
stack it needs, there is no reason to change it. Mirocraft is for the other case: one VPS,
one person, and the wish to install a panel rather than assemble an infrastructure around
one.

---

## Would rather not run a VPS

Everything above is about hosting it yourself. If renting and minding a server
is not what you want, [**MiroHost**](https://mirohost.tech) runs Mirocraft for
you: the panel is already up, the domain and the certificate are configured.

It changes nothing about the panel — it is the code in this repository — and you
can move to your own server whenever you like by copying the data directory.

---

## Requirements

- Linux x86-64 or ARM64 (Debian 11+, Ubuntu 20.04+), or Windows Server 2019+.
- 2 GB of RAM for the panel plus whatever the Minecraft server itself needs. The panel
  takes tens of megabytes; the rest is Java.
- A port for the panel (8080 by default, or 443 with a certificate) and a port per server.
- Docker is optional. The installer will set it up if you let it; without it the panel
  runs servers as processes.

---

## Configuration

`/etc/mirocraft/mirocraft.yaml`, with every key overridable by an environment variable
(`MIROCRAFT_ADDR`, `MIROCRAFT_DATA_DIR` and so on). A fully commented example is in
[`mirocraft.example.yaml`](mirocraft.example.yaml).

Data lives in `/var/lib/mirocraft`: the SQLite database, the server directories, backups
and the downloaded runtimes. Backing up the whole panel is a copy of that directory with
the service stopped.

---

## From source

```bash
git clone https://github.com/collybia/mirocraft
cd mirocraft
make build          # a binary for this OS, web panel embedded
make build-all      # linux/amd64, linux/arm64, windows/amd64
make test
make lint
```

Go 1.26+ and Node 22+ for the web panel. CGO is not needed anywhere — SQLite is
`modernc.org/sqlite` — so cross-compilation works without toolchains.

To install a binary you built rather than a release:

```bash
sudo MIROCRAFT_BINARY=./mirocraft bash installer/install.sh
```

---

## Documentation

- [docs/API.md](docs/API.md) — every endpoint and why it is shaped that way (Russian).
- [docs/API_GUIDE.md](docs/API_GUIDE.md) — a quick start with curl examples (Russian).
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — how it works inside (Russian).
- [docs/SECURITY.md](docs/SECURITY.md) — threat model, what is protected, how to report a vulnerability (Russian).
- [docs/ROADMAP.md](docs/ROADMAP.md) — what is done, and what checking it found (Russian).
- The live spec is at `/api/openapi.yaml`, Swagger UI at `/api/docs`.

---

## License

[AGPL-3.0](LICENSE). Install it, change it, run it for yourself — freely and
without conditions. The one condition applies to anyone running this code as a
**service for other people**: their changes have to be published too. It asks
nothing of someone hosting the panel for themselves.

The name "Mirocraft" and the logo are not covered by the code license.
