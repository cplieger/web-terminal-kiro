# Contributing to vibecli

vibecli is a single Go binary that serves a static web UI and brokers one
`kiro-cli chat` PTY per session (each browser tab is a session with its own
`/ws?session=` connection), via `terminal.NewSessionManager`. There is no chat-history store
and no ACP layer — the browser drives kiro-cli's own TUI through a terminal
stream. This guide covers the things the codebase won't tell you at a glance.

## Architecture at a glance

- `main.go`, `routes.go` — server entry point and route wiring (both at repo
  root, `package main`). `main.go` embeds the web UI with `//go:embed static`.
- `internal/api/` — HTTP response helpers (`WriteJSON`, `WriteJSONStatus`,
  `BadRequest`, `Ok`, …) and the `RequestLogger` middleware (structured slog
  access logging).
- `static-src/` — TypeScript + CSS sources, compiled into `static/`.

vibecli is a thin consumer of the first-party web-terminal libraries: the
terminal engine `web-terminal-engine` (`github.com/cplieger/web-terminal-engine/v2`
server-side, `@cplieger/web-terminal-engine` client-side) and the reference UI
`@cplieger/web-terminal-ui`. Most of "what the terminal does" lives in those
repos, not here. The Go server and TS client share a binary wire protocol, not
code — a wire-format change is a `web-terminal-engine` concern and lands in that
repo, not this one.

Observability is slog-only: `RequestLogger` emits a structured access-log line
per request (method/path/status/duration_ms/request_id/remote). There is no
Prometheus `/metrics` endpoint.

## Generated assets (read before building)

`static/*.js` and `static/style.css` are build artifacts and are
git-ignored — the sources of truth are under `static-src/`. Regenerate them
before `go run .` or `go build .`, otherwise `go:embed` captures a stale or
empty tree:

```sh
go generate ./...   # runs: tsgo --project static-src/tsconfig.json -> static/app.js
```

`go generate` needs `tsgo` (the TypeScript-native preview compiler) on `PATH`.
vibecli ships no local CSS: the bundle is assembled from the vendored
`@cplieger/web-terminal-ui` package. At image-build time the Dockerfile
concatenates the files listed in that package's `css/MANIFEST` into
`static/style.css`. For a local `go run .`, install the package first
(`cd static-src && npm install`), then reproduce the bundle from the repo
root in MANIFEST order:

```sh
UI=static-src/node_modules/@cplieger/web-terminal-ui/css
: > static/style.css
while IFS= read -r f; do
  case "$f" in ''|\#*) continue ;; esac   # skip blanks + comments
  cat "$UI/$f" >> static/style.css
done < "$UI/MANIFEST"
```

## Local dev setup

Run the server directly once assets exist:

```sh
go generate ./...
KWEB_WORK_DIR=/path/to/workdir go run .
```

`KWEB_WORK_DIR` must point at an existing directory (the server exits if it is
missing); `KWEB_ADDR` defaults to `:9848` and `KIRO_CLI_PATH` defaults to
`kiro-cli`. The terminal only works if a real `kiro-cli` binary is reachable —
in production `entrypoint.sh` downloads a Renovate-pinned version on first boot.

`/api/health` reports readiness. Under a bare `go run` it reflects only that the
HTTP listener is up: the kiro-cli readiness gate is env-gated on
`KIRO_CLI_READY_MARKER`, which is left unset locally. In the image the entrypoint
writes that marker only after verifying `kiro-cli --version` matches the pin, so
`/api/health` returns `503 {"reason":"kiro-cli unavailable"}` until kiro-cli is
installed and runnable (the container healthcheck reflects that).

Frontend tooling lives in `static-src/`; run npm commands from there:

```sh
cd static-src
npm install
```

## Running checks

Go (from the repo root):

```sh
go test ./...           # unit + fuzz tests, co-located as *_test.go
golangci-lint run       # lint (config in .golangci.yaml)
golangci-lint fmt       # apply gofumpt + gci formatting
```

`golangci-lint run` reports unformatted files as issues, so the formatters
(`gofumpt` with extra rules, `gci`) are enforced, not just available.

Frontend (from `static-src/`):

```sh
npm run typecheck       # tsgo -project tsconfig.json
npm run test            # vitest --run (node + happy-dom; *.test.ts)
npm run lint:eslint     # eslint .
npm run lint:prettier   # prettier --check ../..
npm run lint:knip       # unused-export / dependency check
```

Vitest defaults to the `node` environment; add `// @vitest-environment
happy-dom` at the top of a test file that needs `window`/`document`. Tests must
assert at least once (`expect.requireAssertions`) and `.only` is forbidden.

## Conventions and gotchas

- **Always use the `internal/api` response helpers.** Never hand-craft JSON
  error bodies (`http.Error` with a JSON string, `w.Write([]byte(...))`). Use
  `WriteJSON`, `WriteJSONStatus`, `Ok`, `BadRequest`, `Conflict`,
  `MethodNotAllowed`.
- **Client-local vs library code.** `static-src/app.ts` is the only client
  source vibecli owns — a single `createTerminal(root, { features: presetAgentTabbed(), theme })` call (the theme is vibecli's purple token set; `presetAgentTabbed` pulls in tabs, the activity monitor, touch toolbar, context menu, clipboard, scroll-to-bottom, predictive echo, connection banner, and animations). The input model,
  IME/composition, predictive echo, viewport, mobile key toolbar, and status
  banner, plus the render / keyboard / scroll / connection layers, all live in
  `@cplieger/web-terminal-ui` (built on `@cplieger/web-terminal-engine`);
  don't reimplement them here.
- **CI workflows are synced, not editable.** Files under `.github/workflows/`
  carry a "Synced from cplieger/ci — DO NOT EDIT" header; the pipeline is
  centralised in `cplieger/ci`. Change behaviour there, not here.
- **kiro-cli install model.** `entrypoint.sh` pins `KIRO_CLI_VERSION` +
  `KIRO_CLI_SHA256` (Renovate-managed). Don't switch to `latest/` URLs, bake
  the binary into the image, or re-enable in-binary auto-update — each breaks
  the pinned-sha / image-tag reproducibility story.
- **The image runs as root by design.** OpenSSH resolves `~` from the passwd
  entry, not `$HOME`, and the Dockerfile wires that entry for root
  (`/config/home`). Don't add a `user:` line to `compose.yaml` or the README
  run example — a non-root UID has no passwd entry, so `git`/`gh` over SSH
  fail with `No user exists for uid …`. Files under `/config` and `/workspace`
  are root-owned on the host as a result.

## Commits and PRs

Branch from `main`, keep changes focused, and open a PR. Commits follow
[Conventional Commits](https://www.conventionalcommits.org/) — git-cliff parses
the type to build release notes and pick the version bump (`feat:`, `fix:`,
`sec:`, `refactor:`/`perf:` ship; `chore`/`ci`/`docs`/`style`/`test` don't). See
`cliff.toml` for the full mapping.

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security issues through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
