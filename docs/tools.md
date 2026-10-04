# Tools

This page covers how Web Terminal for Kiro installs tools beyond kiro-cli, for readers who want language servers, the GitHub CLI or their own tools in every tab.

## The tool list

The image ships kiro-cli, `git` and basic utilities. Everything else is listed in `/config/tools/tools.json`, which the built-in tools engine, the [toolbelt](https://github.com/cplieger/toolbelt) library, reads at every start. Enabled entries are installed into `/config/tools/` and stay there across restarts. Disabled entries wait as templates. A tool install you remove from the list is cleaned up, except an `apt:` package, which stays installed until the container is recreated. There is no settings page for the list. Edit the file and restart the container, or use the commands in [From inside a session](#from-inside-a-session).

## Enable a bundled template

The first start writes the language-server templates and the GitHub CLI, all disabled. Set the ones you want to `"disabled": false` and restart:

```jsonc
{
  "version": 2,
  "tools": {
    "gopls":                      { "disabled": true },   // Go: set false to install (pulls the Go toolchain)
    "typescript-language-server": { "disabled": false },  // TypeScript LSP: enabled (pulls node)
    "pyright":                    { "disabled": true },   // Python LSP
    "rust-analyzer":              { "disabled": true },   // Rust LSP
    "gh":                         { "disabled": true }    // GitHub CLI
  }
}
```

Enabled language servers land on `PATH`, where kiro-cli's [code intelligence](https://kiro.dev/docs/cli/code-intelligence/) finds them. Run `/code init` once per workspace inside a session. `/code status` shows which servers it found.

## Add more tools by name

Any name in the catalog works as an empty entry, and the engine fills in the rest:

```jsonc
"tools": {
  "ripgrep": {},                            // installed at the latest version, then auto-updated
  "shellcheck": { "pin": true },            // pinned: installed once, never auto-bumped
  "jq-custom": {                            // full manual escape hatch
    "source": "manual",
    "version": "1.8.1",
    "install": "curl -fsSL -o ${BIN}/jq https://github.com/jqlang/jq/releases/download/jq-${VERSION}/jq-linux-${ARCH_AMD64_OR_ARM64} && chmod 755 ${BIN}/jq"
  }
}
```

These sources are supported:

- `aqua:owner/repo` binaries, checked against a checksum when upstream publishes one.
- `release:github/owner/repo` GitHub release files, picked by matching your architecture.
- `apt:package` Debian packages, described below.
- `npm:`, `pip:`, `cargo:` and `go:` packages.
- `manual` shell commands, with the `${VERSION}`, `${BIN}` and `${ARCH_*}` placeholders.

## The catalog

Download addresses, checksums and dependencies come from a catalog of about 700 tools, built from the mise and aqua registries by [tool-catalog](https://github.com/cplieger/tool-catalog). A template carries no install commands, so it never goes stale. Dependencies come along on their own. Enabling `typescript-language-server` also installs `node` and the `typescript` package. The server refreshes the catalog at start and then every `TOOL_CATALOG_REFRESH`, keeping the last good copy when a refresh fails. [Configuration](configuration.md) lists the catalog settings.

## OS packages

Declare each Debian package as its own `apt:` entry:

```jsonc
"tools": {
  "gcc":       { "source": "apt:gcc" },
  "libc6-dev": { "source": "apt:libc6-dev" }
}
```

Compared with running `apt-get install` in a session, the entry is kept on the `/config` volume. Recreating the container reinstalls the package instead of losing it, and the entry reports the installed version.

Use plain package names only. Each of these is refused with the reason:

- a version pin such as `pkg=1.2`, or `pkg:arch` or `pkg/release`
- a trailing `-`, which apt reads as a removal
- a name that is not in the package index
- a virtual package such as `awk`, where you name a real provider such as `mawk` instead

Removing an `apt:` entry logs a message and uninstalls nothing. Packages are shared, and the engine will not remove one it cannot prove nothing else needs.

## When tabs open

New tabs wait for the first install pass at start, so the first tab sees every enabled tool on `PATH`. The web page and the health check stay reachable while it runs. If the pass fails, tabs open anyway, the failure is logged, and `/api/health` reports `"tools": "degraded"`.

`/api/health` reports two tool fields, and neither changes the container's healthy or unhealthy state:

- `tools` is `"syncing"` during the start pass, then `"ok"` or `"degraded"` after each install. A repair you make inside the container turns it back to `"ok"` without a restart.
- `tools_missing` counts enabled entries that are not installed yet. Disabled templates never count, and an entry still installing does. The key is absent when the count is unknown, so `0` always means everything is installed.

kiro-cli's own readiness is the separate `status` and `reason` keys.

## From inside a session

The same engine answers on loopback only, so the commands work from a tab or from kiro-cli itself:

```bash
curl -s localhost:9848/api/tools | jq '.tools[] | {name, installed}'
curl -s -X PATCH localhost:9848/api/tools/gopls -d '{"disabled": false}'   # enable + install
curl -s -X POST  localhost:9848/api/tools -d '{"name": "ripgrep"}'         # add from the catalog
```

`GET localhost:9848/api/tools/catalog` reports which catalog is loaded and where it came from. `POST localhost:9848/api/tools/catalog/refresh` refreshes it now. Both are loopback only.

## A warning names files outside /config/tools

The tool list and its state live in `/config/tools/`. Files at `/config/tools.json`, `/config/tools-state.json` and `/config/tool-catalog.cached.json` are ignored. While they exist, the container logs a warning at every start.

To clear the warning, declare the tools you want in `/config/tools/tools.json`, including any tool you added by name. Tools installed while those files were in use keep working on `PATH`, but the engine does not track them. To hand one back, list it in `/config/tools/tools.json` and delete its entry from `/config/tools/bin`, so the engine reinstalls and records it. Then delete the ignored files.
