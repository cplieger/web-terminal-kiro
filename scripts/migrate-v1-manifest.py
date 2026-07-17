#!/usr/bin/env python3
"""Generate a toolbelt v2 tools.json from a retired v1 manifest.

One-shot migration aid for volumes that predate the toolbelt engine
(web-terminal-kiro <= v1.2.x). The new image backs the old file up as
tools.json.v1.bak and seeds a fresh v2 manifest; this script rebuilds
the host's real toolset as v2 entries so custody moves to the engine
WITHOUT changing what is installed:

- exact versions are preserved verbatim (a custody migration must not
  double as an unreviewed mass upgrade);
- v1 auto_update:false becomes pin:true (v1's absent-means-auto default
  maps to pin:false);
- sources are derived, not guessed: `go install <module>@` install
  commands yield go:<module>; known npm installs yield npm:<pkg>;
  catalog-covered binaries map to their aqua sources; anything without
  a clean mapping is carried as a manual entry with its original
  install command (codeql, puppeteer).

The emitted manifest is VALIDATED against a compiled tool catalog
(tool-catalog.json, from the image or a local toolcatalog run): every
aqua source must have an embedded definition, every catalog-named
entry must resolve. Review the output, then apply it (see --help
epilog for the full procedure).

Usage:
    scripts/migrate-v1-manifest.py --v1 tools.json.v1.bak \
        --catalog tool-catalog.json --out tools.json.v2

Offline; stdlib only.
"""

from __future__ import annotations

import argparse
import json
import re
import sys

EPILOG = """procedure (borgcube or any migrating host):
  1. deploy the toolbelt-enabled image; the old manifest is backed up
     to /config/tools.json.v1.bak and a template seed is written
  2. generate:  migrate-v1-manifest.py --v1 tools.json.v1.bak \\
                  --catalog /app/tool-catalog.json --out /tmp/tools.json.v2
     (inside the container the catalog is at /app/tool-catalog.json)
  3. REVIEW the output + warnings, then: cp /tmp/tools.json.v2 /config/tools.json
  4. establish engine custody (reinstalls each tool AT ITS PINNED
     VERSION, recording ownership; existing binaries keep working
     until each install lands):
       for n in $(jq -r '.tools | keys[]' /config/tools.json); do
         curl -sf -X POST "localhost:9848/api/tools/$n/install" >/dev/null && echo "$n queued"
         sleep 1
       done
     (or restart the container: the boot reconcile installs anything
     the PATH probe reports missing, but pre-existing binaries satisfy
     the probe, so explicit installs are what transfer custody)
  5. set APT_PACKAGES on the compose service for the retired apt
     section (the script prints the exact line)
"""

# v1 names that must not ride their install command into v2.
GO_INSTALL_RE = re.compile(r"go install\s+(\S+?)@\$\{VERSION\}")
NPM_SINGLE_RE = re.compile(r"npm install --prefix \$\{TOOLS\}/node -g ([a-z0-9-]+)@\$\{VERSION\}\s*&&\s*ln -sf")

# lsp-section names whose catalog entry replaces the v1 definition
# (catalog carries install + shims + probe; only version/pin survive).
LSP_CATALOG_NAMES = {"tsgo": "tsc-native", "pyrefly": "pyrefly", "gopls": "gopls"}

# binary-section names covered by the compiled catalog (aqua sources).
AQUA_NAMES = {
    "gh", "yq", "age", "dive", "gitleaks", "grype", "hadolint",
    "shellcheck", "syft", "trivy",
}

VALID_VERSION = re.compile(r"^[A-Za-z0-9._+-]{1,100}$")


def warn(msg: str) -> None:
    print(f"WARN: {msg}", file=sys.stderr)


def note(msg: str) -> None:
    print(f"note: {msg}", file=sys.stderr)


def pin_of(entry: dict) -> bool:
    # v1: auto_update absent means auto (follow upstream) = v2 pin:false.
    return entry.get("auto_update", True) is False


def catalog_entry(catalog: dict, name: str) -> dict | None:
    return catalog.get("entries", {}).get(name)


def convert_runtime(name: str, e: dict, out: dict) -> None:
    version = e.get("version", "")
    if name == "go":
        # v1 stored the bare Go version (1.26.5); the aqua:golang/go
        # definition works on upstream TAGS (go1.26.5).
        version = version if version.startswith("go") else f"go{version}"
        out["go"] = {"source": "aqua:golang/go", "version": version, "pin": pin_of(e)}
    elif name == "node":
        out["node"] = {"source": "aqua:nodejs/node", "version": version, "pin": pin_of(e)}
    else:
        warn(f"runtimes.{name}: no mapping; carried as manual")
        convert_manual(name, e, out)


def convert_manual(name: str, e: dict, out: dict) -> None:
    entry: dict = {"source": "manual", "version": e.get("version", ""), "pin": pin_of(e)}
    for src_key, dst_key in (("install", "install"), ("uninstall", "uninstall"), ("shims", "shims"), ("description", "description")):
        if e.get(src_key):
            entry[dst_key] = e[src_key]
    probe = e.get("probe", "")
    # v1 probes could be absolute paths; v2 probes are bin names.
    entry["probe"] = probe.rsplit("/", 1)[-1] if probe else name
    out[name] = entry


def convert_custom(name: str, e: dict, out: dict) -> None:
    install = e.get("install", "")
    m = GO_INSTALL_RE.search(install)
    if m:
        out[name] = {"source": f"go:{m.group(1)}", "version": e.get("version", ""), "pin": pin_of(e)}
        return
    m = NPM_SINGLE_RE.search(install)
    if m and m.group(1) == name:
        out[name] = {"source": f"npm:{name}", "version": e.get("version", ""), "pin": pin_of(e)}
        return
    if name == "stylelint":
        # v1 installed stylelint + stylelint-config-standard in one npm
        # command; v2 models them as two npm entries under the same
        # global prefix (same resolution behavior as the v1 layout).
        cfg = re.search(r"stylelint-config-standard@([0-9.]+)", install)
        out["stylelint"] = {"source": "npm:stylelint", "version": e.get("version", ""), "pin": pin_of(e)}
        if cfg:
            out["stylelint-config-standard"] = {
                "source": "npm:stylelint-config-standard",
                "version": cfg.group(1),
                "pin": True,
                "description": "Shared stylelint config (was bundled into v1's stylelint install).",
            }
        return
    # No clean mapping (codeql bundle, puppeteer wrapper): carry the
    # original install command as a manual entry — behavior-preserving,
    # cleanup can come later.
    note(f"{name}: no source mapping; carried as manual with its v1 install command")
    convert_manual(name, e, out)


def convert_lsp(name: str, e: dict, out: dict, catalog: dict) -> None:
    v2name = LSP_CATALOG_NAMES.get(name)
    if v2name is None or catalog_entry(catalog, v2name) is None:
        warn(f"lsp.{name}: not in the catalog; carried as manual")
        convert_manual(name, e, out)
        return
    cat = catalog_entry(catalog, v2name) or {}
    version = e.get("version", "")
    src = cat.get("source", "")
    if src.startswith("go:"):
        # gopls: the catalog's go: source resolves module tags (vX.Y.Z).
        out[v2name] = {"source": src, "version": version, "pin": pin_of(e)}
    else:
        # Manual catalog entries (tsc-native, pyrefly): install command,
        # shims, and probe HYDRATE from the catalog; only intent fields
        # are written so catalog improvements keep applying.
        out[v2name] = {"version": version, "pin": pin_of(e)}


def convert_binary(name: str, e: dict, out: dict, catalog: dict) -> None:
    cat = catalog_entry(catalog, name)
    if name in AQUA_NAMES and cat and str(cat.get("source", "")).startswith("aqua:"):
        out[name] = {"source": cat["source"], "version": e.get("version", ""), "pin": pin_of(e)}
        return
    note(f"binary.{name}: not aqua-covered; carried as manual with its v1 install command")
    convert_manual(name, e, out)


def convert_apt(v1: dict) -> tuple[dict, list[str]]:
    """apt section: yamllint moves to pip; the rest become APT_PACKAGES."""
    out: dict = {}
    apt_pkgs: list[str] = []
    for name in v1.get("apt", {}):
        if name == "yamllint":
            out["yamllint"] = {"source": "pip:yamllint"}
        else:
            apt_pkgs.append(name)
    return out, apt_pkgs


def validate(tools: dict, catalog: dict) -> int:
    """Dry-run checks mirroring the engine's install-time requirements."""
    failures = 0
    aqua_sources = {e.get("source"): e for e in catalog.get("entries", {}).values()}
    for name, e in sorted(tools.items()):
        src = e.get("source", "")
        version = e.get("version", "")
        if version and not VALID_VERSION.match(version):
            warn(f"{name}: version {version!r} fails the engine's charset")
            failures += 1
        if not src:
            # Sparse: must hydrate from the catalog.
            if catalog_entry(catalog, name) is None:
                warn(f"{name}: sparse entry but not in the catalog")
                failures += 1
            continue
        kind = src.split(":", 1)[0]
        if kind == "aqua":
            hit = aqua_sources.get(src)
            if not hit or not hit.get("aqua"):
                warn(f"{name}: {src} has no embedded aqua definition in the catalog")
                failures += 1
        elif kind == "manual":
            if not e.get("install"):
                warn(f"{name}: manual without an install command")
                failures += 1
        elif kind not in ("npm", "pip", "cargo", "go"):
            warn(f"{name}: unknown source kind {kind!r}")
            failures += 1
    return failures


def convert(v1: dict, catalog: dict) -> tuple[dict, list[str]]:
    tools: dict = {}
    for name, e in v1.get("runtimes", {}).items():
        convert_runtime(name, e, tools)
    for name, e in v1.get("binary", {}).items():
        convert_binary(name, e, tools, catalog)
    for name, e in v1.get("custom", {}).items():
        convert_custom(name, e, tools)
    for name, e in v1.get("lsp", {}).items():
        convert_lsp(name, e, tools, catalog)
    apt_tools, apt_pkgs = convert_apt(v1)
    tools.update(apt_tools)

    # v1 enabled:false entries become disabled templates (recorded, not
    # installed) instead of being dropped: same recorded-intent shape.
    for section in ("runtimes", "binary", "custom", "lsp"):
        for name, e in v1.get(section, {}).items():
            v2name = LSP_CATALOG_NAMES.get(name, name)
            if e.get("enabled") is False and v2name in tools:
                tools[v2name]["disabled"] = True
    return tools, apt_pkgs


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__.split("\n", 1)[0],
        epilog=EPILOG,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("--v1", required=True, help="the backed-up v1 manifest (tools.json.v1.bak)")
    ap.add_argument("--catalog", required=True, help="compiled tool-catalog.json (from the image: /app/tool-catalog.json)")
    ap.add_argument("--out", required=True, help="output path for the v2 manifest")
    args = ap.parse_args()

    with open(args.v1, encoding="utf-8") as f:
        v1 = json.load(f)
    with open(args.catalog, encoding="utf-8") as f:
        catalog = json.load(f)
    if "tools" in v1 and "version" in v1:
        print("ERROR: input already looks like a v2 manifest", file=sys.stderr)
        return 2

    tools, apt_pkgs = convert(v1, catalog)
    failures = validate(tools, catalog)

    manifest = {
        "version": 2,
        "_comment": [
            "Migrated from the retired v1 manifest by migrate-v1-manifest.py:",
            "versions preserved verbatim, v1 auto_update:false -> pin:true.",
            "Entries without a source hydrate install knowledge (command,",
            "shims, dependencies) from the image's tool catalog.",
        ],
        "tools": tools,
    }
    with open(args.out, "w", encoding="utf-8") as f:
        json.dump(manifest, f, indent=2)
        f.write("\n")

    pinned = sum(1 for e in tools.values() if e.get("pin"))
    print(f"wrote {args.out}: {len(tools)} entries ({pinned} pinned)", file=sys.stderr)
    if apt_pkgs:
        print(f'\ncompose env for the retired apt section:\n  APT_PACKAGES="{" ".join(sorted(apt_pkgs))}"', file=sys.stderr)
    if failures:
        print(f"\n{failures} validation failure(s) — fix before applying", file=sys.stderr)
        return 1
    print("validation clean", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
