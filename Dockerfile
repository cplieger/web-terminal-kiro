# check=error=true

# --- Builder stage: compile Go server + fetch xterm.js vendor files ---
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:b6e2a152f22a40ff69d92cb397223c906017e1391a73c952b588e51af8883bf8 AS builder

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl jq openssl xz-utils && rm -rf /var/lib/apt/lists/*

# Go for building the web server.
# renovate: datasource=golang-version depName=golang
ARG TARGETARCH
ARG TARGETOS=linux
ARG BUILDARCH
ARG GO_VERSION=1.26.3
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${BUILDARCH}.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# tsgo (TypeScript 7 native preview) for compiling the browser client.
# Matches the approach in apps/vibekit's Dockerfile: pull Microsoft's
# typescript-go preview tarball, invoke the binary with --project on
# static-src/tsconfig.json, emit lands in static/app.js for go:embed.
# The same tsgo binary is also installed in the final stage for LSP.
# Tracks @typescript/native-preview's `beta` dist-tag (Microsoft's curated
# stabler channel) rather than the daily `latest`; the platform-specific
# linux-x64 tarball is published in lockstep at the same version string.
# See .github/renovate.json for the followTag rule.
# renovate: datasource=npm depName=@typescript/native-preview
ARG TSGO_VERSION=7.0.0-dev.20260421.2
RUN TSGO_ARCH=$([ "$BUILDARCH" = "arm64" ] && echo "arm64" || echo "x64") && \
    curl -fsSL \
      "https://registry.npmjs.org/@typescript/native-preview-linux-${TSGO_ARCH}/-/native-preview-linux-${TSGO_ARCH}-${TSGO_VERSION}.tgz" \
    | tar -xz -C /tmp

# Nerd Font. kiro-cli's diff UI uses nerd-font private-use-area
# glyphs (line markers, file-type icons). System monospace fonts
# don't carry these, so they render as tofu (black squares) in
# the terminal display. Bundling one Mono-width Nerd Font + serving
# it via @font-face fixes that. JetBrainsMono is ~3.8 MB
# uncompressed; with go:embed it grows the vibecli binary by that
# much and ships gzipped over the wire (~900 KB to the browser).
# renovate: datasource=github-releases depName=ryanoasis/nerd-fonts
ARG NERDFONT_VERSION=v3.4.0

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . ./

# Fetch Nerd Font for the monospace terminal display.
RUN mkdir -p static/vendor/fonts && \
    curl -fsSL "https://github.com/ryanoasis/nerd-fonts/releases/download/${NERDFONT_VERSION}/Monaspace.tar.xz" \
      | tar -xJ -C static/vendor/fonts \
          MonaspiceNeNerdFontMono-Regular.otf \
          MonaspiceNeNerdFontMono-Bold.otf \
          MonaspiceNeNerdFontMono-Italic.otf \
          MonaspiceNeNerdFontMono-BoldItalic.otf

# Compile client TypeScript. Must run before the binary build because
# main.go's `//go:embed static` captures static/ at `go build` time.
# tsconfig.json's outDir is "../static", so tsgo writes static/app.js
# directly into the embed tree.
RUN /tmp/package/lib/tsgo --project static-src/tsconfig.json

# Concatenate per-feature CSS splits into the served bundle. Mirrors
# lib/shell/build-css.sh; inlined here because the Docker build context
# is apps/vibecli/ and can't reach the repo-level lib/. Behavior must
# stay in sync with that script: skip blank lines and #-comments, cat
# each listed file (paths relative to manifest dir) into the output.
RUN set -eu; \
    : > static/style.css; \
    while IFS= read -r line || [ -n "$line" ]; do \
        case "$line" in ''|\#*) continue ;; esac; \
        cat "static-src/css/${line}" >> static/style.css; \
    done < static-src/css/MANIFEST

# Build the Go binary with static assets embedded via go:embed.
# CGO disabled so the binary runs on any glibc.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /vibecli .

# --- Final stage: minimal runtime with kiro-cli + git + gh ---
FROM debian:trixie-slim@sha256:b6e2a152f22a40ff69d92cb397223c906017e1391a73c952b588e51af8883bf8

ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Baked-in dependencies. kiro-cli itself is downloaded on first boot
# by entrypoint.sh (licensing prevents us from baking it into the
# image); everything else is stable utility surface vibecli or the
# interactive user needs:
#   - ca-certificates + curl + unzip: kiro-cli installer + HTTPS trust
#   - git + gh: source control from inside the terminal
#   - openssh-client: gh over ssh, git over ssh
#   - jq + less: standard kiro-cli diagnostic dependencies
#
# Session persistence is handled by vibecli's own VT screen
# (internal/vt) — the server keeps an authoritative cell buffer and
# replays the current snapshot on each WS reconnect. No external
# multiplexer (tmux/dtach) is required.
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    ca-certificates \
    curl \
    git \
    jq \
    less \
    openssh-client \
    unzip \
    && rm -rf /var/lib/apt/lists/*

# Language servers kiro-cli's workspace_manager spawns on startup.
# Without them the Ink TUI silently stalls for ~15 minutes on first
# paint. Both ship as native binaries (no Node runtime needed).
# renovate: datasource=github-releases depName=facebook/pyrefly
ARG PYREFLY_VERSION=1.0.0
# renovate: datasource=npm depName=@typescript/native-preview
ARG TSGO_VERSION=7.0.0-dev.20260421.2
ARG TARGETARCH
RUN PYREFLY_ARCH=$([ "$TARGETARCH" = "arm64" ] && echo "arm64" || echo "x86_64") && \
    TSGO_ARCH=$([ "$TARGETARCH" = "arm64" ] && echo "arm64" || echo "x64") && \
    curl -fsSL "https://github.com/facebook/pyrefly/releases/download/${PYREFLY_VERSION}/pyrefly-linux-${PYREFLY_ARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin pyrefly && \
    chmod +x /usr/local/bin/pyrefly && \
    printf '#!/bin/sh\nexec /usr/local/bin/pyrefly lsp\n' > /usr/local/bin/pyright && chmod +x /usr/local/bin/pyright && \
    cp /usr/local/bin/pyright /usr/local/bin/pyright-langserver && \
    curl -fsSL "https://registry.npmjs.org/@typescript/native-preview-linux-${TSGO_ARCH}/-/native-preview-linux-${TSGO_ARCH}-${TSGO_VERSION}.tgz" \
      | tar -xz -C /usr/local/bin --strip-components=2 --wildcards 'package/lib/tsgo' 'package/lib/lib*.d.ts' && \
    chmod +x /usr/local/bin/tsgo && \
    printf '%s\n' '#!/bin/sh' 'exec /usr/local/bin/tsgo --lsp --stdio' \
      > /usr/local/bin/typescript-language-server && \
    chmod +x /usr/local/bin/typescript-language-server

# Developer tools (gh, etc.) are installed dynamically from
# /config/tools.json on first boot via setup-tools.sh. This lets
# users customize their toolset without rebuilding the image.

# kiro-cli installs under $HOME/.local. Home is under /config so the
# install survives container restarts.
ENV HOME=/config/home
ENV PATH="/tools/bin:/config/tools/bin:/config/home/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
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

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=30s \
    CMD curl -sf http://127.0.0.1:9848/api/health || exit 1

ENTRYPOINT ["/opt/vibecli/entrypoint.sh"]
