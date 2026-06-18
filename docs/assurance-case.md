# Security assurance case — vibecli

This extends the fleet-wide
[default assurance case](https://github.com/cplieger/.github/blob/main/assurance-case.md)
with the threat model specific to `vibecli`. Read that first. vibecli is
**pre-1.0**; the [roadmap](../ROADMAP.md) lists the work in progress.

## What this is

A minimal browser terminal: a Go web server that streams a single kiro-cli PTY
over a WebSocket to a browser renderer. As a terminal to an interactive shell/
agent, its **intended capability is full command execution** — so, like vibekit,
the security model is about reaching it, not about sandboxing what it can do.

## Security model

vibecli is a **trusted-operator tool behind a network/auth boundary**. In the
homelab it is reachable only on the internal network (behind the reverse proxy).
A reachable, authenticated session is, by design, a shell — that is the product.
The control is "only the operator can reach vibecli."

## Threats and mitigations

| Threat                                            | Mitigation                                                                                  | Evidence                                 |
| ------------------------------------------------- | ------------------------------------------------------------------------------------------- | ---------------------------------------- |
| Unauthorised access to the PTY (i.e. to a shell)  | auth middleware on the server; network/auth boundary (LAN gate + reverse proxy)             | `internal/api/middleware.go`, deployment |
| WebSocket hijacking / malformed frames            | origin/auth checks on the WS upgrade; hardened wire decoding (shared `vterm` wire protocol) | middleware, vterm decoder tests          |
| Malformed terminal/wire input crashing the server | property + fuzz suite on the PTY/wire surface                                               | fuzz targets (10), weekly fuzz           |
| Stale/empty embedded UI shipped                   | CI image smoke test starts the container and asserts it serves                              | image smoke test (CI docker job)         |

## Residual risks (stated plainly)

- **Pre-1.0**, with active work on mobile UI and WebSocket robustness (roadmap).
- An authenticated session is a shell by design; network/auth isolation is the
  control and is a deployment responsibility. Do not expose vibecli to untrusted
  networks.

Report vulnerabilities privately per
[SECURITY.md](https://github.com/cplieger/.github/blob/main/SECURITY.md).
