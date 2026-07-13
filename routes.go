package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cplieger/web-terminal-engine/v2/terminal"
	"github.com/cplieger/webhttp"
)

type routeDeps struct {
	staticFS fs.FS
	ready    *atomic.Bool
	workDir  string
	// kiroReadyMarker, when non-empty, is a file the entrypoint touches only
	// after verifying a runnable, correctly-versioned kiro-cli is installed
	// (see entrypoint.sh). /api/health Stats it to reflect web-terminal-kiro's core
	// dependency. Empty (e.g. `go run`/tests outside the container) skips the
	// gate, preserving pure-listener readiness semantics.
	kiroReadyMarker string
	cmd             []string
}

func registerRoutes(mux *http.ServeMux, deps *routeDeps) (*terminal.SessionManager, error) {
	sub, err := fs.Sub(deps.staticFS, "static")
	if err != nil {
		return nil, err
	}
	etags, err := buildETags(sub)
	if err != nil {
		return nil, err
	}
	mux.Handle("/", cacheHeaders(etags, http.FileServer(http.FS(sub))))

	// factory builds one kiro-cli chat session per tab: an independent PTY-backed
	// process (deps.cmd = kiro-cli chat) with its own VT screen and scrollback, so
	// opening a tab launches a fresh instance. Scrollback 5000 covers a /chat
	// transcript restore on reconnect (matches the client store's retained-line
	// cap). WithKeepUnfocused pins the process to the DEC 1004 "unfocused" state so
	// kiro-cli keeps emitting its focus-gated OSC 9 notifications (which drive the
	// classifier) even though no browser tab claims focus (design 7.2);
	// web-terminal-server deliberately does NOT use this, since a generic
	// shell/editor wants real focus reporting.
	//
	// No TERM_PROGRAM override here: the engine now advertises TERM_PROGRAM=
	// iTerm.app (>= 3.6.6), which puts kiro-cli in its OSC 9;4 progress allowlist
	// (driving the tab's "working" dot) and enables DEC 2026 synchronized output.
	// That is the same identity web-terminal-server gets, so web-terminal-kiro inherits it
	// from the engine rather than pinning its own (it formerly set WezTerm; both
	// unlock kiro-cli, and iTerm.app additionally covers other agents like Claude
	// Code). Anything the engine can't render (inline images) is consumed silently.
	factory := func(id string) *terminal.Handler {
		return terminal.NewHandler(deps.cmd,
			terminal.WithWorkDir(deps.workDir),
			terminal.WithScrollbackCapacity(5000),
			terminal.WithKeepUnfocused(),
			terminal.WithLogger(slog.Default().With("session", id)),
		)
	}

	mgr := terminal.NewSessionManager(factory,
		terminal.WithManagerLogger(slog.Default()),
		terminal.WithStatusClassifier(classifyStatus),
	)

	// Mount only /ws (the session WebSocket, dispatched on ?session=<id>). As with
	// the previous single-handler setup we deliberately do NOT expose the engine's
	// /debug/raw (raw PTY ring) or /debug/screen (full VT buffer) on this
	// unauthenticated surface. Same posture as web-terminal-server.
	mux.Handle("/ws", mgr.WebSocketHandler())

	// Session REST API. createRateLimit gates POST /api/sessions so a caller
	// cannot fork kiro-cli processes without bound (the limiter bounds create
	// churn); a kiro-cli chat is heavy, so this matters more here than for a
	// plain shell. Mounted at the exact path and the subtree so /api/sessions and
	// /api/sessions/{id} both route.
	limitedREST := createRateLimit(mgr.RESTHandler())
	mux.Handle("/api/sessions", limitedREST)
	mux.Handle("/api/sessions/", limitedREST)
	// The status SSE is a more specific path than the REST subtree, so the mux
	// routes it here rather than to the REST DELETE /{id} pattern.
	mux.Handle("/api/sessions/events", mgr.EventsHandler())

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		unready := func(reason string) {
			webhttp.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unready",
				"reason": reason,
			})
		}
		if !deps.ready.Load() {
			unready("starting up or shutting down")
			return
		}
		// kiro-cli readiness (env-gated via kiroReadyMarker). web-terminal-kiro's core job
		// is spawning kiro-cli chat PTYs, but the HTTP listener comes up even when
		// the first-boot install failed (degraded-not-dead start). Reflect the
		// entrypoint's boot-time verdict here so a broken kiro-cli surfaces as
		// unready to `docker ps` and the monitoring probe. A cheap Stat keeps this
		// spawn-free: kiro-cli was verified once at boot via --version, never
		// relaunched per probe (a per-probe spawn of a heavy PTY process would be
		// an anti-pattern). This is a READINESS signal, not liveness — under
		// `restart: unless-stopped` nothing restarts on the resulting unhealthy
		// state, so there is no restart loop; if ever run under Swarm/k8s, wire
		// this to a readinessProbe, not a livenessProbe.
		if deps.kiroReadyMarker != "" {
			if _, err := os.Stat(deps.kiroReadyMarker); err != nil {
				unready("kiro-cli unavailable")
				return
			}
		}
		webhttp.WriteJSON(w, map[string]string{"status": "ok"})
	})

	return mgr, nil
}

// classifyStatus maps kiro-cli's OSC 9 notification text to a latched session
// status for the tab activity dots: "Response complete" at the end of an agent
// turn latches the done (green) state, and "Permission required" when a tool
// call is blocked on approval latches the needs-input (amber) state (confirmed
// against the pinned 2.11.0 build). A new working phase (the OSC 9;4 progress
// signal, enabled by the factory's TERM_PROGRAM) clears the latch. Any other
// message is ignored. This mapping is the only kiro-cli-specific coupling; the
// engine stays generic (a plain shell server sets no classifier and derives
// working/idle from output activity).
func classifyStatus(msg string) (string, bool) {
	switch msg {
	case "Response complete":
		return terminal.StatusDone, true
	case "Permission required":
		return terminal.StatusInput, true
	default:
		// Any OSC 9 text the pinned kiro-cli build does not emit for turn-end or
		// tool-approval. If a kiro-cli bump reworded "Response complete"/"Permission
		// required", every notification lands here and the per-tab status dots
		// silently stop latching. This Debug line is the only runtime trace of that
		// drift (invisible at the default Info level; raise the level to diagnose a
		// "status dots stopped working" report after a version bump).
		slog.Debug("unrecognized kiro-cli OSC 9 notification; tab status dots will not latch (kiro-cli notification wording may have changed on a version bump)", "message", msg)
		return "", false
	}
}

// Create-rate-limit tuning: a token bucket with a small burst (open several tabs
// at once) refilling at a steady rate, so sustained create churn is throttled
// while normal use is unaffected. Each kiro-cli chat is a heavy process, so
// bounding create churn matters. Mirrors web-terminal-server.
const (
	createBurst    = 6
	createInterval = time.Second // interval to accrue one create token
)

// createRateLimit gates POST /api/sessions (session creation) behind a shared
// token bucket via webhttp.RateLimiter, so a caller cannot fork kiro-cli
// processes without bound; list (GET) and close (DELETE) pass through
// unthrottled. The bucket is process-wide (it bounds aggregate create churn),
// which is what matters when each kiro-cli chat is a heavy process.
func createRateLimit(next http.Handler) http.Handler {
	return webhttp.RateLimiter(createBurst, createInterval,
		webhttp.WithRateLimitWhen(func(r *http.Request) bool {
			return r.Method == http.MethodPost && r.URL.Path == "/api/sessions"
		}),
		webhttp.WithRateLimitError("rate_limited", "session creation rate exceeded"),
	)(next)
}

// buildETags walks the embedded static tree once and computes a stable
// content-hash ETag per file. embed.FS reports a zero ModTime, so
// http.FileServer emits no validator on its own; precomputing a hash gives
// http.ServeContent an If-None-Match target so unchanged assets answer 304
// instead of re-downloading on every load.
func buildETags(sub fs.FS) (map[string]string, error) {
	etags := make(map[string]string)
	err := fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := fs.ReadFile(sub, p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(b)
		etags[p] = fmt.Sprintf(`"%x"`, sum[:])
		return nil
	})
	return etags, err
}

// cacheHeaders applies cache policy:
//   - fonts (/vendor/fonts/**): immutable, 30 days. The Monaspace .otf
//     files are large (~2.4 MB each, ~9.4 MB total) and their glyphs are
//     fixed for a given vendored web-terminal-ui version, so immutable
//     avoids re-downloading them on every visit. CAVEAT: the filenames
//     are NOT content-addressed (no hash), and immutable suppresses even
//     reload revalidation — a font whose bytes change under the SAME
//     filename is served stale for up to 30 days. A font swap must change
//     the path/filename (or hash it at vendor time) to bust the cache.
//   - everything else (HTML/JS/CSS, ~1–30 KB modules): no-cache +
//     must-revalidate so deployments take effect immediately. A per-file
//     content-hash ETag (precomputed by buildETags) is set so unchanged
//     files revalidate with a cheap 304 (no re-download): embed.FS reports
//     a zero ModTime, so http.ServeContent emits no Last-Modified validator
//     on its own and would otherwise re-send the full body on every load.
//     The hash changes only when the bundle bytes change, busting the cache
//     exactly on a deploy and keeping the TS engine bundle in lockstep with
//     the server wire protocol.
func cacheHeaders(etags map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/vendor/fonts/"):
			w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		default:
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
			if name == "" {
				name = "index.html"
			}
			if etag, ok := etags[name]; ok {
				w.Header().Set("ETag", etag)
			}
		}
		next.ServeHTTP(w, r)
	})
}
