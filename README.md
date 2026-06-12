# vibecli

[![CI](https://github.com/cplieger/vibecli/actions/workflows/ci.yaml/badge.svg)](https://github.com/cplieger/vibecli/actions/workflows/ci.yaml)
[![GitHub release](https://img.shields.io/github/v/release/cplieger/vibecli)](https://github.com/cplieger/vibecli/releases)
[![Image Size](https://ghcr-badge.egpl.dev/cplieger/vibecli/size)](https://github.com/cplieger/vibecli/pkgs/container/vibecli)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Debian](https://img.shields.io/badge/base-Debian_trixie-A81D33?logo=debian)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/vibecli/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/vibecli)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](LICENSE)

A minimal browser terminal for the Kiro CLI — `kiro-cli` in a tab, nothing more.

## What it is

Vibecli is a single Go binary that serves a static web UI and brokers a PTY for one `kiro-cli` process per session. Unlike its sister app [vibekit](https://github.com/cplieger/vibekit), there is no ACP bridge, no chat protocol, and no chat-history persistence — the browser drives `kiro-cli`'s own TUI directly through the terminal stream, the same as an SSH session. Terminal state lives only in the server's in-memory VT buffer and is replayed to the browser on reconnect.

The terminal engine (VT500 screen buffer + WebSocket PTY handler on the server, renderer/keyboard/mouse/wire-decoder in the browser) is the shared [`@cplieger/vterm`](https://github.com/cplieger/vterm) library; vibecli adds only its own predictive echo, IME handling, and viewport/status UI.

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
      - ./config:/config        # kiro-cli auth/state, tools
      - ./workspace:/workspace  # your repos
    restart: unless-stopped
```

`kiro-cli` is downloaded and pinned on first boot (it is not redistributed in the image, per the AWS Customer Agreement). Open `http://localhost:9848`, authenticate `kiro-cli`, and you have a terminal.

## Security

Network-exposed: put it behind an authenticating reverse proxy — a browser tab here is a shell with filesystem access to `/workspace`. Observability is `slog`-only (structured access log; no metrics endpoint). Debian `trixie-slim` base (a shell + the `kiro-cli` subprocess are required). Images are published with cosign signatures and SBOM attestations.

## License

GPL-3.0. See [LICENSE](LICENSE).
