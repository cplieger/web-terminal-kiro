package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cplieger/vibecli/internal/api"
)

// TestDebugRoutesNotExposed pins the security posture of registerRoutes: the
// engine's terminal handler is mounted at /ws ONLY (via mux.Handle), never via
// term.RegisterRoutes, which would also wire the unauthenticated /debug/raw
// (raw PTY ring) and /debug/screen (full VT buffer) on this network surface.
// Regressing to RegisterRoutes re-opens the leak this test guards against.
func TestDebugRoutesNotExposed(t *testing.T) {
	mux := http.NewServeMux()
	var ready atomic.Bool
	deps := &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ok")}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)

	// /ws must be registered as its own pattern.
	if _, pat := mux.Handler(httptest.NewRequest(http.MethodGet, "/ws", http.NoBody)); pat != "/ws" {
		t.Errorf("/ws routed to pattern %q, want \"/ws\"", pat)
	}

	// /debug/* must NOT be registered — an unregistered path falls through to
	// the "/" file-server catch-all, so its matched pattern must not be itself.
	for _, p := range []string{"/debug/raw", "/debug/screen"} {
		if _, pat := mux.Handler(httptest.NewRequest(http.MethodGet, p, http.NoBody)); pat == p {
			t.Errorf("%s is registered (pattern %q); /debug routes must not be exposed", p, pat)
		}
	}
}

// TestHealthEndpoint_reflectsReadiness pins the /api/health readiness gate:
// before ready is set the endpoint returns 503 (so a reverse proxy or
// orchestrator holds traffic during startup and shutdown), and once ready it
// returns 200. The atomic flag is the only thing that flips the branch.
func TestHealthEndpoint_reflectsReadiness(t *testing.T) {
	mux := http.NewServeMux()
	var ready atomic.Bool
	deps := &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ok")}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", http.NoBody))
		return rec
	}

	if rec := get(); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before ready: status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	ready.Store(true)
	if rec := get(); rec.Code != http.StatusOK {
		t.Errorf("after ready: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestCacheHeaders_setsPolicyByPath pins cacheHeaders' two-branch policy:
// files under /vendor/fonts/ are immutable for 30 days (content-addressed by
// filename) while everything else is no-cache so deploys take effect at once.
// The trailing slash in the prefix is load-bearing -- "/vendor/fonts-list.json"
// must NOT be treated as a font -- and the middleware must call next in every
// branch.
func TestCacheHeaders_setsPolicyByPath(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantCache string
	}{
		{name: "font is immutable", path: "/vendor/fonts/iosevka.woff2", wantCache: "public, max-age=2592000, immutable"},
		{name: "nested font is immutable", path: "/vendor/fonts/sub/x.woff2", wantCache: "public, max-age=2592000, immutable"},
		{name: "html is no-cache", path: "/index.html", wantCache: "no-cache, must-revalidate"},
		{name: "root is no-cache", path: "/", wantCache: "no-cache, must-revalidate"},
		{name: "js bundle is no-cache", path: "/app.js", wantCache: "no-cache, must-revalidate"},
		{name: "vendor non-font prefix is no-cache", path: "/vendor/fonts-list.json", wantCache: "no-cache, must-revalidate"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, http.NoBody)

			cacheHeaders(nil, next).ServeHTTP(rec, req)

			if !called {
				t.Errorf("path %q: next handler was not called", tc.path)
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.wantCache {
				t.Errorf("Cache-Control for %q = %q, want %q", tc.path, got, tc.wantCache)
			}
		})
	}
}

// TestStaticETagRevalidation pins the embedded-bundle revalidation contract
// promised by cacheHeaders' godoc: embed.FS reports a zero ModTime, so
// http.FileServer emits no validator on its own and every full load would
// re-download the body. buildETags precomputes a content-hash ETag that
// cacheHeaders sets on the default (non-font) branch, so GET / returns a quoted
// ETag and a conditional GET with a matching If-None-Match answers 304 with an
// empty body instead of re-sending the bundle. Mirrors the sibling
// web-terminal-server's TestStaticHandlerETagAndRevalidation.
func TestStaticETagRevalidation(t *testing.T) {
	mux := http.NewServeMux()
	var ready atomic.Bool
	deps := &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("<!doctype html>")}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)

	// First load: the response carries a quoted content-hash ETag.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want %d", rec.Code, http.StatusOK)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET /: no ETag header; the browser cannot revalidate the embedded bundle and re-downloads it every load")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag %q is not a quoted strong validator", etag)
	}

	// Conditional reload: a matching If-None-Match answers 304 with no body.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set("If-None-Match", etag)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional GET /: status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("304 response body = %q, want empty", body)
	}
}

// TestBuildETags_isContentAddressedSHA256 pins the content-addressing contract
// in buildETags' godoc: the ETag is the quoted hex SHA-256 of each file's
// CONTENT, so it changes exactly when the bundle bytes change and busts the
// cache on deploy. TestStaticETagRevalidation only round-trips the ETag the
// server emits, so it stays green even if buildETags hashed the path or a
// constant; this test hashes nothing itself (digests are hardcoded) so it
// cannot share a bug with the code under test.
func TestBuildETags_isContentAddressedSHA256(t *testing.T) {
	const (
		htmlBody = "<!doctype html>\n"
		jsBody   = "export const x = 1;\n"
		// Quoted lowercase-hex SHA-256 of each body, precomputed offline.
		wantHTML = `"335fca8574f060eea24ebcdae6b78f32414f5de03da1084fd0e73d710768e3a9"`
		wantJS   = `"b40dedde60828bf61d1fadbfc3bb7ea2e0421e9511d22f1b5fb44ae5ba07dbb3"`
	)
	// buildETags walks the already-Sub'd tree, so keys carry no "static/" prefix.
	sub := fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte(htmlBody)},
		"app.js":      &fstest.MapFile{Data: []byte(jsBody)},
		"vendor/c.js": &fstest.MapFile{Data: []byte(jsBody)},
	}

	etags, err := buildETags(sub)
	if err != nil {
		t.Fatalf("buildETags: %v", err)
	}

	if etags["index.html"] != wantHTML {
		t.Errorf("ETag[index.html] = %q, want %q (quoted sha256 of the file CONTENT)", etags["index.html"], wantHTML)
	}
	if etags["app.js"] != wantJS {
		t.Errorf("ETag[app.js] = %q, want %q", etags["app.js"], wantJS)
	}
	// Identical bytes under a different path must hash identically -- this is
	// what dies when a mutant hashes the path instead of the content.
	if etags["vendor/c.js"] != wantJS {
		t.Errorf("ETag[vendor/c.js] = %q, want %q (same bytes as app.js must hash identically)", etags["vendor/c.js"], wantJS)
	}
	// Distinct contents must differ, or a deploy that changes the bundle would
	// not bust the cache.
	if etags["index.html"] == etags["app.js"] {
		t.Errorf("distinct contents shared ETag %q; the cache would never bust on deploy", etags["index.html"])
	}
	// Directories get no entry (the d.IsDir() guard).
	if _, ok := etags["vendor"]; ok {
		t.Error(`directory "vendor" got an ETag entry; buildETags must skip directories`)
	}
	if _, ok := etags["."]; ok {
		t.Error(`root "." got an ETag entry; buildETags must skip directories`)
	}
}

// TestSSEStreamsThroughRequestLogger is the regression guard for the tab status
// stream behind vibecli's own middleware. RequestLogger wraps most requests in
// statusRecorder (which implements neither Flush nor Unwrap); if the SSE path
// were wrapped, the engine's flush probe would fail and the stream would 500. It
// is instead bypassed like /ws, so it gets the raw ResponseWriter. This drives
// /api/sessions/events through the full CrossOriginProtection + RequestLogger
// chain and asserts the stream opens (200 + text/event-stream) and flushes an
// event.
func TestSSEStreamsThroughRequestLogger(t *testing.T) {
	mux := http.NewServeMux()
	var ready atomic.Bool
	ready.Store(true)
	deps := &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ok")}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	id, err := mgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	handler := http.NewCrossOriginProtection().Handler(api.RequestLogger(mux))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/events", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/sessions/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE must bypass the status recorder, not 500)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") && strings.Contains(line, id) {
			return // the initial-sync event flushed through the middleware
		}
	}
	t.Fatalf("SSE stream delivered no data through RequestLogger (scan err: %v)", sc.Err())
}

// TestCreateRateLimit pins the create throttle: a burst of POST /api/sessions is
// allowed, then further creates are 429'd, while GET (list) is never limited. It
// exercises createRateLimit directly with a stub so it does not fork real
// kiro-cli processes.
func TestCreateRateLimit(t *testing.T) {
	var restHit atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		restHit.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	h := createRateLimit(next)
	post := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", http.NoBody))
		return rec.Code
	}

	allowed := 0
	for range int(createBurst) {
		if post() == http.StatusOK {
			allowed++
		}
	}
	if allowed != int(createBurst) {
		t.Errorf("allowed %d creates in the burst, want %d", allowed, int(createBurst))
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Errorf("create past the burst = %d, want 429", code)
	}

	// GET (list) is never rate-limited, even after the create burst is spent.
	restHit.Store(false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", http.NoBody))
	if !restHit.Load() {
		t.Error("GET /api/sessions was blocked by the create rate limiter")
	}
}
