# check=error=true

# --- Builder stage: compile Go server + vendor the web-terminal engine/UI TS ---
FROM debian:trixie-slim@sha256:28de0877c2189802884ccd20f15ee41c203573bd87bb6b883f5f46362d24c5c2 AS builder

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# hadolint ignore=DL3008
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    ca-certificates curl xz-utils && rm -rf /var/lib/apt/lists/*

# Go for building the web server. GO_VERSION and both per-arch tarball sha256
# pins move together, maintained by Renovate: the golang-amd64 / golang-arm64
# custom datasources (in cplieger/.github default.json) read go.dev's
# ?mode=json and rewrite GO_SHA256_AMD64 / GO_SHA256_ARM64 alongside GO_VERSION
# in one grouped "golang toolchain" PR (CI builds amd64 and arm64 natively).
# The `# go<version>` trailer on each sha line is the anchor Renovate uses to
# resolve that arch's digest — do not hand-edit; Renovate owns these lines.
# renovate: datasource=golang-version depName=golang
ARG GO_VERSION=1.26.5
# renovate: datasource=custom.golang-amd64 depName=golang-amd64
ARG GO_SHA256_AMD64=5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053  # go1.26.5
# renovate: datasource=custom.golang-arm64 depName=golang-arm64
ARG GO_SHA256_ARM64=fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49  # go1.26.5
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) GO_SHA256="$GO_SHA256_AMD64" ;; \
      arm64) GO_SHA256="$GO_SHA256_ARM64" ;; \
      *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
    esac && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" && \
    printf '%s  /tmp/go.tar.gz\n' "$GO_SHA256" | sha256sum -c - && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}"

# tsgo (TypeScript 7 native preview) for compiling the browser client.
# Matches the approach in apps/vibekit's Dockerfile: pull Microsoft's
# typescript-go preview tarball, invoke the binary with --project on
# static-src/tsconfig.json, emit lands in static/app.js for go:embed.
# The tsgo binary is used here at build time only (TS compilation).
# Runtime LSPs are no longer baked — they install on-demand via
# setup-tools.sh from /config/tools.json.
# Tracks @typescript/native-preview's `latest` dist-tag (Microsoft's curated
# stabler channel) rather than the daily `latest`; the platform-specific
# linux-x64 tarball is published in lockstep at the same version string.
# See .github/renovate.json for the followTag rule.
# renovate: datasource=npm depName=@typescript/native-preview
ARG TSGO_VERSION=7.0.0-dev.20260615.1
# sha256 of the platform-specific tsgo tarball, per arch. Update both alongside
# TSGO_VERSION (the linux-x64 and linux-arm64 packages publish in lockstep).
ARG TSGO_SHA256_X64=214b2ff1bae02f902b2ddc90f9599ad7991e6f333ccb97046489170cd5865d1e
ARG TSGO_SHA256_ARM64=373161c691fcf7af07936daecbc065ce5343c4a15c47d87e879f86de921b4d1a
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) TSGO_ARCH="x64";   TSGO_SHA256="$TSGO_SHA256_X64" ;; \
      arm64) TSGO_ARCH="arm64"; TSGO_SHA256="$TSGO_SHA256_ARM64" ;; \
      *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
    esac && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o /tmp/tsgo.tgz \
      "https://registry.npmjs.org/@typescript/native-preview-linux-${TSGO_ARCH}/-/native-preview-linux-${TSGO_ARCH}-${TSGO_VERSION}.tgz" && \
    printf '%s  /tmp/tsgo.tgz\n' "$TSGO_SHA256" | sha256sum -c - && \
    tar -xz -C /tmp -f /tmp/tsgo.tgz && \
    rm /tmp/tsgo.tgz

# Nerd Font. kiro-cli's diff UI uses nerd-font private-use-area
# glyphs (line markers, file-type icons). System monospace fonts
# don't carry these, so they render as tofu (black squares) in
# the terminal display. Bundling one Mono-width Nerd Font + serving
# it via @font-face fixes that. JetBrainsMono is ~3.8 MB
# uncompressed; with go:embed it grows the vibecli binary by that
# much and ships gzipped over the wire (~900 KB to the browser).
# renovate: datasource=github-releases depName=ryanoasis/nerd-fonts
ARG NERDFONT_VERSION=v3.4.0
# sha256 of Monaspace.tar.xz for this tag. GitHub release assets are MUTABLE (a
# retag can swap the bytes under a fixed tag), so this gate is the real
# integrity anchor here. Update alongside NERDFONT_VERSION.
ARG NERDFONT_SHA256=5fdb97828e1a23fd28ea5ed0e7d15cdebb77ef079aaa48b93f1526764b40ef8c

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . ./

# Fetch Nerd Font for the monospace terminal display.
RUN mkdir -p static/vendor/fonts && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o /tmp/mona.tar.xz \
      "https://github.com/ryanoasis/nerd-fonts/releases/download/${NERDFONT_VERSION}/Monaspace.tar.xz" && \
    printf '%s  /tmp/mona.tar.xz\n' "$NERDFONT_SHA256" | sha256sum -c - && \
    tar -xJ -C static/vendor/fonts -f /tmp/mona.tar.xz \
        MonaspiceNeNerdFontMono-Regular.otf \
        MonaspiceNeNerdFontMono-Bold.otf \
        MonaspiceNeNerdFontMono-Italic.otf \
        MonaspiceNeNerdFontMono-BoldItalic.otf && \
    rm /tmp/mona.tar.xz

# Fetch the engine + UI TypeScript from the npm registry. Both publish TS
# source only (no precompiled JS) — same pattern as @cplieger/reactive,
# matching how local TS files in static-src/ are treated. Extracted side by
# side under static-src/node_modules/@cplieger so tsgo's bundler resolution
# finds the engine when compiling the UI's `@cplieger/web-terminal-engine` import.
# Integrity note: unlike the Go, tsgo, and Nerd Font fetches above, the two
# @cplieger npm tarballs are NOT sha256-pinned. npm published package versions
# are immutable (the registry refuses to re-publish an existing version), and
# both are first-party packages fetched over pinned TLS (--proto '=https'
# --tlsv1.2 below). The residual risk (a registry-side byte-swap or a
# first-party npm account takeover) is accepted here; add per-package sha256
# ARGs + `sha256sum -c` for parity with the tsgo gate if that risk is later
# deemed in scope (at the cost of a manual sha bump on each engine/UI release).
# renovate: datasource=npm depName=@cplieger/web-terminal-engine
ARG CPLIEGER_WEB_TERMINAL_ENGINE_VERSION=2.2.1
# renovate: datasource=npm depName=@cplieger/web-terminal-ui
ARG CPLIEGER_WEB_TERMINAL_UI_VERSION=3.4.1
RUN mkdir -p static-src/node_modules/@cplieger/web-terminal-engine static-src/node_modules/@cplieger/web-terminal-ui && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o /tmp/engine.tgz "https://registry.npmjs.org/@cplieger/web-terminal-engine/-/web-terminal-engine-${CPLIEGER_WEB_TERMINAL_ENGINE_VERSION}.tgz" && \
    tar -xz -C static-src/node_modules/@cplieger/web-terminal-engine --strip-components=1 -f /tmp/engine.tgz && \
    rm /tmp/engine.tgz && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o /tmp/ui.tgz "https://registry.npmjs.org/@cplieger/web-terminal-ui/-/web-terminal-ui-${CPLIEGER_WEB_TERMINAL_UI_VERSION}.tgz" && \
    tar -xz -C static-src/node_modules/@cplieger/web-terminal-ui --strip-components=1 -f /tmp/ui.tgz && \
    rm /tmp/ui.tgz

# Compile client TypeScript and the engine + UI libs in a single layer.
# Must run before the binary build because main.go's `//go:embed static`
# captures static/ at `go build` time.
#
# Step 1: tsgo --project compiles app TS — tsconfig.json's outDir is
# "../static", so tsgo writes static/app.js directly into the embed tree.
# The lib import (`@cplieger/web-terminal-ui`) is preserved in the emit as a
# bare specifier; the browser resolves it via the importmap in
# static/index.html.
#
# Steps 2+3: compile the engine and UI TS source into static/vendor/ so the
# browser can fetch the compiled JS via the importmap. Internal imports (the
# UI's bare `@cplieger/web-terminal-engine` and both packages' relative `./*.js`) are
# preserved and resolve via the importmap + vendored dirs at runtime.
RUN mapfile -t ui_ts < <(find static-src/node_modules/@cplieger/web-terminal-ui/src -name '*.ts') && \
    /tmp/package/lib/tsgo --project static-src/tsconfig.json && \
    /tmp/package/lib/tsgo \
        --module ESNext \
        --target ESNext \
        --moduleResolution bundler \
        --outDir static/vendor/cplieger-web-terminal-engine \
        --rootDir static-src/node_modules/@cplieger/web-terminal-engine/src \
        --skipLibCheck \
        --strict \
        static-src/node_modules/@cplieger/web-terminal-engine/src/*.ts && \
    /tmp/package/lib/tsgo \
        --module ESNext \
        --target ESNext \
        --moduleResolution bundler \
        --outDir static/vendor/cplieger-web-terminal-ui \
        --rootDir static-src/node_modules/@cplieger/web-terminal-ui/src \
        --skipLibCheck \
        --strict \
        "${ui_ts[@]}"

# Concatenate the UI package's per-feature CSS splits into the served bundle.
# Behavior: skip blank lines and #-comments, cat each listed file
# (paths relative to manifest dir) into the output.
RUN set -eu; \
    : > static/style.css; \
    while IFS= read -r line || [ -n "$line" ]; do \
        case "$line" in ''|\#*) continue ;; esac; \
        cat "static-src/node_modules/@cplieger/web-terminal-ui/css/${line}" >> static/style.css; \
    done < static-src/node_modules/@cplieger/web-terminal-ui/css/MANIFEST

# Build the Go binary with static assets embedded via go:embed.
# CGO disabled so the binary runs on any glibc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /vibecli .

# --- Final stage: minimal runtime with kiro-cli + git ---
FROM debian:trixie-slim@sha256:28de0877c2189802884ccd20f15ee41c203573bd87bb6b883f5f46362d24c5c2

ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Baked-in dependencies. kiro-cli itself is downloaded on first boot
# by entrypoint.sh (licensing prevents us from baking it into the
# image); everything else is stable utility surface vibecli or the
# interactive user needs:
#   - ca-certificates + curl + unzip: kiro-cli installer + HTTPS trust
#   - git: source control from inside the terminal (gh is NOT baked; it
#     is opt-in via /config/tools.json)
#   - openssh-client: git over ssh (and gh over ssh once gh is enabled)
#   - jq + less: standard kiro-cli diagnostic dependencies
#   - libasound2: kiro-cli dlopens libasound.so.2 at runtime. It is NOT
#     declared in kiro-cli's .deb manifest (which only lists GUI deps:
#     libayatana-appindicator3-1, libwebkit2gtk-4.1-0, libgtk-3-0) — it
#     gets pulled transitively via libwebkit2gtk on the desktop install.
#     Our headless zip variant bypasses apt entirely, so without this
#     line kiro-cli aborts on first invocation with
#     "error while loading shared libraries: libasound.so.2: cannot open
#     shared object file". Surfaced once kiro-cli >= 2.6 started
#     exercising the code path.
#
# Session persistence is handled by vibecli's own VT screen
# (internal/vt) — the server keeps an authoritative cell buffer and
# replays the current snapshot on each WS reconnect. No external
# multiplexer (tmux/dtach) is required.
# hadolint ignore=DL3008
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    jq \
    less \
    libasound2 \
    openssh-client \
    unzip \
    xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Language servers are no longer baked into the image. They install
# on-demand via setup-tools.sh from /config/tools.json (same mechanism
# as vibekit). Users who want TS/Python/Go LSPs add them to their
# tools.json with the shim pattern — see the reference config for an
# example. This saves ~32 MB off the compressed image and eliminates
# the daily tsgo-bump Docker rebuild churn.

# Developer tools (gh, etc.) are installed dynamically from
# /config/tools.json on first boot via setup-tools.sh. This lets
# users customize their toolset without rebuilding the image.

# kiro-cli installs under $HOME/.local. Home is under /config so the
# install survives container restarts.
ENV HOME=/config/home
ENV PATH="/config/tools/bin:/config/tools/go/bin:/config/tools/runtimes/go/bin:/config/tools/runtimes/node/bin:/config/home/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
ENV GOROOT="/config/tools/runtimes/go"
ENV GOPATH="/config/tools/go"
ENV GOBIN="/config/tools/go/bin"
ENV KIRO_CLI_PATH=/config/tools/bin/kiro-cli
ENV KWEB_WORK_DIR=/workspace
ENV KWEB_ADDR=:9848

# Repoint root's pw_dir to /config/home so OpenSSH (which resolves "~"
# via getpwuid, NOT $HOME) reads and writes ~/.ssh/known_hosts under
# the persisted volume. Without this, every container recreation wipes
# the host-key cache.
RUN sed -i 's|^root:x:0:0:root:/root:|root:x:0:0:root:/config/home:|' /etc/passwd

COPY --from=builder /vibecli /app/vibecli
COPY --chmod=755 entrypoint.sh /opt/vibecli/entrypoint.sh
COPY --chmod=755 setup-tools.sh /opt/vibecli/setup-tools.sh
COPY tools.json /opt/vibecli/tools.json

WORKDIR /workspace
EXPOSE 9848

# start-period is sized for the default (all tools disabled in tools.json) boot:
# kiro-cli download only. entrypoint.sh runs setup-tools.sh FOREGROUND before the
# server binds, and each tool install is budgeted up to INSTALL_TIMEOUT=600s, so a
# host that enables heavy installs (e.g. the ~190 MB Go runtime) can exceed this
# window. Under `restart: unless-stopped` the container just shows a transient
# "unhealthy" and self-heals; under a liveness-acting orchestrator, raise
# start-period (compose healthcheck override) to cover the enabled tools' install
# time.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=180s \
    CMD curl -sf http://127.0.0.1:9848/api/health || exit 1

ENTRYPOINT ["/opt/vibecli/entrypoint.sh"]
