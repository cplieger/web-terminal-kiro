package main

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync/atomic"

	"github.com/cplieger/vibecli/internal/api"
	"github.com/cplieger/web-terminal-engine/terminal"
)

type routeDeps struct {
	staticFS fs.FS
	ready    *atomic.Bool
	workDir  string
	cmd      []string
}

func registerRoutes(mux *http.ServeMux, deps *routeDeps) (*terminal.Handler, error) {
	sub, err := fs.Sub(deps.staticFS, "static")
	if err != nil {
		return nil, err
	}
	etags, err := buildETags(sub)
	if err != nil {
		return nil, err
	}
	mux.Handle("/", cacheHeaders(etags, http.FileServer(http.FS(sub))))

	// Retain enough scrollback to cover a kiro-cli /chat session restore (which
	// dumps the whole transcript at once) so it survives a reconnect without a
	// trim. Matches the web-terminal-engine client store's retained-line cap.
	term := terminal.NewHandler(deps.cmd,
		terminal.WithWorkDir(deps.workDir),
		terminal.WithScrollbackCapacity(5000),
	)
	// Mount only /ws (term.ServeHTTP delegates to the WebSocket handler). We
	// deliberately do NOT call term.RegisterRoutes, which would also expose the
	// engine's /debug/raw (last 16 KB of raw PTY bytes) and /debug/screen (full
	// VT buffer) on this unauthenticated network surface. Same posture as
	// web-terminal-server (main.go: mux.Handle("/ws", term)).
	mux.Handle("/ws", term)

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		if !deps.ready.Load() {
			api.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unready",
				"reason": "starting up or shutting down",
			})
			return
		}
		api.WriteJSON(w, map[string]string{"status": "ok"})
	})

	return term, nil
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
