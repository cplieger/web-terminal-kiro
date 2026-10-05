# Contributing to web-terminal-kiro

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Scope

This repo owns the server, its kiro-cli install settings and the page setup in `static-src/app.ts`. A change to how kiro-cli is downloaded, verified, selected or activated belongs in [pinstall](https://github.com/cplieger/pinstall).

A change to how the terminal draws, takes input, scrolls or reconnects belongs in [web-terminal-ui](https://github.com/cplieger/web-terminal-ui).

A change to the wire protocol, or to how a route the engine mounts answers, belongs in [web-terminal-engine](https://github.com/cplieger/web-terminal-engine). This app picks up all three through a version bump.

## Rules

- Every refusal the app writes goes through `webhttp.WriteError` with a snake_case code, like `loopback_only` and `not_ready`. The `/api/health` and rescan status documents use `webhttp.WriteJSONStatus` instead. A plain `http.Error` body keeps the status but drops the JSON envelope and the request ID that matches the access-log line.
- A rejected environment value is logged by its variable name, never by its value, because a compose expansion mistake can put a credential in any variable. Some library parsers log the raw value of a bad input, so screen a value before you hand it to one, as `parseCatalogRefresh` does for `TOOL_CATALOG_REFRESH`.
- `static-src/eslint.config.base.mjs` is a hand-kept copy of the synced root `eslint.config.base.mjs`, and ESLint reads only the copy. When a sync changes the root file, copy it into `static-src/` too. A lint rule for this repo alone goes in `static-src/eslint.config.mjs`.
- `static/app.js`, `static/style.css` and `static/vendor/` are gitignored build output. `go:embed static` embeds whatever the folder holds, so a local build serves what the last run left there.
- `bash scripts/dev-build.sh` regenerates all three and builds `web-terminal-kiro-dev-bin` from web-terminal-engine and web-terminal-ui checked out beside this repo. `ENGINE_DIR` and `UI_DIR` point it elsewhere. Run `npm install` in `static-src/` first.
- `go generate ./...` rebuilds only `static/app.js`. A build after it serves a page with no terminal, because the library modules under `static/vendor/` are missing.
- `scripts/dev-build.sh` leaves a `go.work` that points at your engine checkout, and copies of both checkouts' sources in `static-src/node_modules`. Run it again after you change either checkout. To build and test against the published versions again, delete `go.work` and run `npm ci` in `static-src/`.

## Checks

`npm run test:e2e` in `static-src/` checks the served page in a real browser, and CI does not run it. Run it after a change to `static/index.html` or `static-src/app.ts`, against the dev binary or a running image at `PLAYWRIGHT_BASE_URL` (default `http://127.0.0.1:9848`).

With `KIRO_CLI_VERSION` or `KIRO_CLI_TOOLS_DIR` unset, the server installs nothing and runs the `kiro-cli` on your `PATH`. To exercise the managed install without a download, set both, then give it a complete version folder and a digest of the right shape:

```sh
export KIRO_CLI_TOOLS_DIR=/tmp/kweb-tools KIRO_CLI_VERSION=2.14.2
# Any 64 lowercase hex characters. On arm64, set KIRO_CLI_SHA256_ARM64 instead.
export KIRO_CLI_SHA256=0000000000000000000000000000000000000000000000000000000000000000
V="$KIRO_CLI_TOOLS_DIR/kiro-cli-versions/$KIRO_CLI_VERSION"
mkdir -p "$V"
cp /path/to/kiro-cli /path/to/kiro-cli-chat "$V/"
printf '%s\n' "$KIRO_CLI_VERSION" >"$V/.complete"
WORK_DIR=/path/to/folder ./web-terminal-kiro-dev-bin
```

With both variables set but no digest for this architecture, the install manager is never built and every session is refused as `kiro-cli unavailable`. Both binaries can be shell scripts. `kiro-cli --version` must print `kiro-cli 2.14.2` and `kiro-cli settings app.disableAutoupdates true` must exit 0, or readiness stays withheld. A failure of any other `settings` call only logs a warning.
