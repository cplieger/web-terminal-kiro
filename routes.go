package main

import (
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/cplieger/vibecli/internal/api"
	"github.com/cplieger/vibecli/internal/auth"
	"github.com/cplieger/vibecli/internal/metrics"
	"github.com/cplieger/vterm/terminal"
)

type routeDeps struct {
	staticFS fs.FS
	ready    *atomic.Bool
	workDir  string
	cliPath  string
	cmd      []string
}

func registerRoutes(mux *http.ServeMux, deps *routeDeps) (*terminal.Handler, error) {
	sub, err := fs.Sub(deps.staticFS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", cacheHeaders(http.FileServer(http.FS(sub))))

	term := terminal.NewHandler(deps.cmd, terminal.WithWorkDir(deps.workDir))
	term.RegisterRoutes(mux)

	authH := auth.NewHandler(deps.cliPath, auth.DefaultTimeoutPolicy())
	authH.RegisterRoutes(mux)

	mux.HandleFunc("/api/theme", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			api.MethodNotAllowed(w, r)
			return
		}
		api.Ok(w)
	})

	mux.HandleFunc("/metrics", metrics.Handler())

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

// cacheHeaders applies cache policy:
//   - fonts (/vendor/fonts/**): immutable, 30 days — content-addressed by
//     filename; if a font ever changes we'd ship a new filename
//   - everything else (HTML/JS/CSS): no-cache + must-revalidate so
//     deployments take effect immediately. Files are tiny (~10KB) so the
//     revalidation roundtrip is cheap relative to the cost of stale code.
func cacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/vendor/fonts/"):
			w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		default:
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		next.ServeHTTP(w, r)
	})
}
