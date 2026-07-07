# vibecli

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/vibecli/badges/size.json)](https://github.com/cplieger/vibecli/pkgs/container/vibecli)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Debian](https://img.shields.io/badge/base-Debian-A81D33?logo=debian)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/vibecli/badges/coverage.json)](https://github.com/cplieger/vibecli/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/vibecli/badges/mutation.json)](https://github.com/cplieger/vibecli/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13223/badge)](https://www.bestpractices.dev/projects/13223)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/vibecli/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/vibecli)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/vibecli/releases)

A minimal browser terminal for the **Kiro CLI**: run `kiro-cli` in a browser tab, on your desktop or your phone.

vibecli gives each browser tab its own `kiro-cli` session over a live PTY stream and renders kiro-cli's real terminal UI verbatim, the way an SSH session would, with no chat layer, history store, or translation in between.

What sets it apart from a typical browser terminal: the screen is **real browser text, not a canvas**, so scrolling and text selection are native; it is **touch-first with multiple tabs**, as usable on a phone as on a laptop; and sessions **survive sleep and network drops**: the screen and scrollback are replayed on reconnect, so you never lose your place.

Published as a container image: `ghcr.io/cplieger/vibecli` (amd64 + arm64).

## ⚠️ It is a remote shell

A browser tab here is an interactive shell with access to your files under `/workspace` and to kiro-cli's stored credentials. Anyone who can reach the port can use it, and vibecli has **no built-in authentication**. Before exposing it beyond your own machine, do one (ideally both) of:

- put it behind an authenticating reverse proxy (Caddy forward-auth, oauth2-proxy, Authentik, …), and/or
- keep the published port on loopback or a private network.

The server logs a warning at startup when it binds a non-loopback address.

## Run

```yaml
# compose.yaml
services:
  vibecli:
    image: ghcr.io/cplieger/vibecli:latest
    ports:
      - "9848:9848"
    volumes:
      - ./config:/config        # kiro-cli auth, tools, settings
      - ./workspace:/workspace  # your repos
    restart: unless-stopped
```

Open <http://localhost:9848> and sign in from the terminal (`kiro-cli login`). Open more tabs for more sessions.

vibecli runs as root so `git`, `gh`, and SSH work; don't add a `user:` line, and expect files under the mounts to be root-owned on the host.

## Configuration

The image ships working defaults; most setups only pick a port and a volume.

| Variable | Default | Purpose |
| --- | --- | --- |
| `KWEB_ADDR` | `:9848` | Listen address (`host:port`). |
| `KWEB_WORK_DIR` | `/workspace` | Directory each terminal session starts in (must exist). |
| `TRUSTED_PROXIES` | _(unset)_ | Reverse-proxy CIDRs / bare IPs whose `X-Forwarded-For` the access log trusts to resolve `client_ip`. See [Access-log client IP](#access-log-client-ip). |

- **Port:** `9848` (HTTP + WebSocket).
- **Volumes:** `/config` persists kiro-cli auth/tokens, installed tools, settings, and `~/.ssh` + git config; `/workspace` is your repositories / working directory.
- **Health:** the image's healthcheck reports healthy only once the server is up **and** kiro-cli is installed and runnable, so a failed first-boot install shows as `unhealthy` in `docker ps` instead of a terminal that silently errors.

kiro-cli itself is pinned and downloaded on first boot (it is not redistributed inside the image); newer versions arrive by pulling a newer image tag.

### Access-log client IP

The access log records a `client_ip` per request. By default (`TRUSTED_PROXIES` unset) it logs the direct socket peer and ignores any `X-Forwarded-For` header, so the logged IP cannot be spoofed; that's the correct choice when vibecli is directly exposed. When you run it behind a reverse proxy the socket peer is the proxy, not the user, so set `TRUSTED_PROXIES` to the proxy's address(es), a comma-separated list of CIDRs or bare IPs (e.g. `TRUSTED_PROXIES=10.0.0.0/8,192.0.2.10`), and the log resolves the real client from a trusted `X-Forwarded-For`. Only a request whose socket peer is inside the set has its `X-Forwarded-For` trusted (spoof-safe); a malformed entry is logged and skipped rather than aborting startup.

## Features

Everything below works on a phone as well as a desktop.

**A faithful terminal**, powered by [web-terminal-engine](https://github.com/cplieger/web-terminal-engine):

- Full 16 / 256 / 24-bit truecolor and every text attribute (bold, italic, underline, reverse, strikethrough, …), box-drawing, and wide CJK characters.
- Mouse support and clickable **OSC 8 hyperlinks** (bare URLs are auto-linked too).
- Desktop **notifications** and **progress** indicators (OSC 9 / OSC 9;4).
- Full-screen apps (`vim`, `htop`, `less`, `man`) run on the alternate screen, with your scrollback restored on exit.
- Bracketed paste, selectable cursor styles, the Kitty keyboard protocol, and clipboard writes from CLI apps (OSC 52).

**Made for touch**, via the [web-terminal-ui](https://github.com/cplieger/web-terminal-ui) front end:

- **Multiple tabs**: open, close, drag to reorder, plus a swipeable mobile tab switcher.
- An on-screen **key toolbar** (Tab, Esc, arrows, Enter, and a sticky-Ctrl modifier) for keys a phone keyboard lacks.
- Native **text selection**, copy/paste, and a **long-press / right-click context menu**.
- **Predictive echo** so typing feels instant over slow links, tap-to-focus, and a scroll-to-bottom control with auto-follow.
- **Per-tab status dots**: see at a glance which session is working, done, or waiting for input.
- IME/composition support, keyboard accessibility, theming, and reduced-motion support.

**Resilient by default:**

- Auto-reconnect with screen + scrollback replay after laptop sleep, network drops, or proxy timeouts.
- Input sent during an outage is re-delivered on reconnect (no lost or duplicated output), and a restarted server is detected and cleanly resynced.

## Works with the whole kiro-cli TUI

Because vibecli drives kiro-cli's own terminal UI directly, every kiro-cli feature works with no extra setup, including queue steering (`Ctrl+S`), goal-driven runs (`/goal`), and turn rewind (`/rewind`). On a phone, the shortcuts that need modifier keys are reachable through the on-screen toolbar (sticky-Ctrl, then the letter).

## Tools

vibecli ships kiro-cli, `git`, and base utilities. Everything else is declared in a JSON manifest at `/config/tools.json` and installed into `/config/tools/` on boot, persisting across restarts. There is no management UI; you edit the manifest and restart the container.

**Enable a bundled tool.** The manifest ships several entries, all disabled by default. Set `"enabled": true` on the ones you want:

```jsonc
{
  "runtimes": { "go": { "enabled": true } },   // Go toolchain (needed by gopls)
  "binary": {
    "gh":            { "enabled": true },       // GitHub CLI
    "golangci-lint": { "enabled": true }
  },
  "lsp": {
    "tsgo":    { "enabled": true },             // TypeScript language server
    "pyrefly": { "enabled": true },             // Python language server
    "gopls":   { "enabled": true }              // Go; also enable runtimes.go above
  }
}
```

Language servers are picked up by kiro-cli's code intelligence automatically. `gopls` needs the Go toolchain, so enable both `lsp.gopls` and `runtimes.go`.

**Add your own tool.** Add an entry under a section (`binary`, `custom`, `lsp`, `runtimes`) with an `install` shell command that drops the tool into `${BIN}` (`/config/tools/bin`, which is on `PATH`):

```jsonc
{
  "custom": {
    "ripgrep": {
      "enabled": true,
      "version": "14.1.1",
      "install": "curl -fsSL https://github.com/BurntSushi/ripgrep/releases/download/${VERSION}/ripgrep-${VERSION}-x86_64-unknown-linux-musl.tar.gz | tar -xz -C ${BIN} --strip-components=1 ripgrep-${VERSION}-x86_64-unknown-linux-musl/rg"
    }
  }
}
```

`${VERSION}` and `${BIN}` are substituted at install time. The shipped `gh` and `golangci-lint` entries are good templates; they show the architecture placeholders for multi-arch downloads and the optional integrity check: add a `url` plus a per-arch `sha256` map (`{"amd64": "…", "arm64": "…"}`) and vibecli verifies the download before installing (pin the entry with `"auto_update": false` so the checksum keeps matching).

Editing this repo's `tools.json` only seeds a fresh `/config` volume; on an existing install, edit the manifest inside `/config`.

## How it fits together

```text
kiro-cli                      one process per browser tab
   │  PTY
web-terminal-engine           Go PTY + VT engine ──► vibecli server
   │  binary wire protocol over WebSocket
web-terminal-engine + web-terminal-ui   ──► your browser (renderer + touch UI)
```

vibecli is deliberately small: an HTTP + WebSocket server around the engine, the kiro-cli install, and a structured access log. The terminal itself is the shared web-terminal libraries.

## Related projects

- [vibekit](https://github.com/cplieger/vibekit): the sister app, a chat-first Kiro web UI (chat history, MCP, agent tools) instead of a raw terminal.
- [web-terminal-engine](https://github.com/cplieger/web-terminal-engine): the terminal engine (Go PTY/VT + TypeScript renderer) behind vibecli.
- [web-terminal-ui](https://github.com/cplieger/web-terminal-ui): the touch-first browser UI.
- [web-terminal-server](https://github.com/cplieger/web-terminal-server): a generic browser terminal for any command, built on the same engine.

## Contributing

Build, test, and layout notes are in [CONTRIBUTING.md](CONTRIBUTING.md).

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0. See [LICENSE](LICENSE).
