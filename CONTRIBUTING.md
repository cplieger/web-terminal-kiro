# Contributing to vibecli

vibecli is a single Go binary that serves a static web UI and brokers one
`kiro-cli chat` PTY per WebSocket connection. There is no chat-history store
and no ACP layer — the browser drives kiro-cli's own TUI through a terminal
stream. This guide covers the things the codebase won't tell you at a glance.

## Architecture at a glance

- `main.go`, `routes.go` — server entry point and route wiring (both at repo
  root, `package main`). `main.go` embeds the web UI with `//go:embed static`.
- `internal/api/` — HTTP response helpers (`WriteJSON`, `BadRequest`, `Ok`, …),
  the `RequestLogger` middleware (structured slog access logging), and the
  terminal output sanitisers (`StripANSI`, `SanitizeOutput`).
- `internal/auth/` — `/api/whoami`, `/api/login`, `/api/logout`; shells out to
  the bundled `kiro-cli` for identity, no persisted state.
- `static-src/` — TypeScript + CSS sources, compiled into `static/`.

vibecli is a thin consumer of one first-party shared library: `vterm`
(`github.com/cplieger/vterm` server-side, `@cplieger/vterm` client-side), the
terminal engine. Most of "what the terminal does" lives in `vterm`, not here.
The Go server and TS client share a binary wire protocol, not code — a
wire-format change is a `vterm` concern and lands in that repo, not this one.

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
The CSS bundle has no `go generate` step: the Dockerfile concatenates the files
listed in `static-src/css/MANIFEST` into `static/style.css` at image-build
time. For local styling, concatenate them in manifest order yourself, e.g.:

```sh
( cd static-src/css && cat 00-tokens.css 01-reset.css 02-app.css ) > static/style.css
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
- **Sanitise subprocess output.** Anything captured from `kiro-cli` that is
  echoed to clients or logs goes through `api.SanitizeOutput` first — it strips
  ANSI escapes and hidden Unicode used for prompt injection.
- **Client-local vs library code.** Only vibecli-specific UI lives in
  `static-src/` (`predict.ts`, `composition.ts`, `viewport.ts`, `status.ts`,
  `app.ts`). The render / keyboard / scroll / connection layers are imported
  from `@cplieger/vterm`; don't reimplement them here.
- **CI workflows are synced, not editable.** Files under `.github/workflows/`
  carry a "Synced from cplieger/ci — DO NOT EDIT" header; the pipeline is
  centralised in `cplieger/ci`. Change behaviour there, not here.
- **kiro-cli install model.** `entrypoint.sh` pins `KIRO_CLI_VERSION` +
  `KIRO_CLI_SHA256` (Renovate-managed). Don't switch to `latest/` URLs, bake
  the binary into the image, or re-enable in-binary auto-update — each breaks
  the pinned-sha / image-tag reproducibility story.

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
