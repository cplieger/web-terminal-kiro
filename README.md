# vibecli

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/vibecli/badges/size.json)](https://github.com/cplieger/vibecli/pkgs/container/vibecli)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Debian](https://img.shields.io/badge/base-Debian-A81D33?logo=debian)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/vibecli)](https://goreportcard.com/report/github.com/cplieger/vibecli)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/vibecli/badges/coverage.json)](https://github.com/cplieger/vibecli/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/vibecli/badges/mutation.json)](https://github.com/cplieger/vibecli/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13223/badge)](https://www.bestpractices.dev/projects/13223)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/vibecli/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/vibecli)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/vibecli/releases)

A minimal browser terminal for the Kiro CLI — `kiro-cli` in a tab, nothing more.

## What it is

Vibecli is a single Go binary that serves a static web UI and brokers a PTY for one `kiro-cli` process per session. Unlike its sister app [vibekit](https://github.com/cplieger/vibekit), there is no ACP bridge, no chat protocol, and no chat-history persistence — the browser drives `kiro-cli`'s own TUI directly through the terminal stream, the same as an SSH session. Terminal state lives only in the server's in-memory VT buffer and is replayed to the browser on reconnect.

The terminal engine (VT500 screen buffer + WebSocket PTY handler on the server, renderer/keyboard/mouse/wire-decoder in the browser) is the shared [`@cplieger/web-terminal-engine`](https://github.com/cplieger/web-terminal-engine) library, and the touch-first browser UI built on it — predictive echo, IME handling, viewport, status banner, and the mobile key toolbar — is the [`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui) reference UI. vibecli is the thinnest possible consumer: `static-src/app.ts` is a single `mount()` call, with almost nothing terminal-related held locally.

## Features

- **Raw `kiro-cli` TUI in the browser** — full terminal UI, reconnect with screen + scrollback replay (survives sleep/network blips).
- **Persistent state** on a single `/config` bind mount: `kiro-cli` auth/tokens, tools, and settings.
- **Pinned `kiro-cli`** — version + sha256 are Renovate-tracked in `entrypoint.sh`; bumps land via image rebuild (auto-update disabled for reproducibility).

## Run it

```yaml
# compose.yaml
services:
  vibecli:
    image: ghcr.io/cplieger/vibecli:latest
    ports:
      - "9848:9848"
    volumes:
      - "/opt/appdata/vibecli:/config"
      - "/opt/appdata/vibecli/workspace:/workspace"  # your repos
    restart: unless-stopped
```

The container runs as **root** by design — the image gives root a home on the persistent volume (`/config/home`), and OpenSSH resolves `~` from that passwd entry (not `$HOME`), so `kiro-cli`, `git`, and `gh` work over SSH. Running as a non-root UID (`user: "1000:1000"`) has no passwd entry and breaks `git`/`gh` over SSH with `No user exists for uid 1000`. The entrypoint creates `/config` and `/workspace` on first boot, so there's no host-side setup; files written there are root-owned on the host — `chown` them to your user if you need to reach the checkouts from outside the container.

`kiro-cli` is downloaded and pinned on first boot (it is not redistributed in the image, per the AWS Customer Agreement). Open `http://localhost:9848`, authenticate `kiro-cli`, and you have a terminal.

## Security

Network-exposed: put it behind an authenticating reverse proxy — a browser tab here is a shell with filesystem access to `/workspace`. Observability is `slog`-only (structured access log; no metrics endpoint). Debian base (a shell + the `kiro-cli` subprocess are required). Images are published with cosign signatures and SBOM attestations.

## License

GPL-3.0. See [LICENSE](LICENSE).
