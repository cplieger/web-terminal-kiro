package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cplieger/web-terminal-engine/v2/terminal"
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

// TestHealthEndpoint_reflectsKiroCliReadiness pins the kiro-cli readiness gate
// added for the deferred readiness-decoupled-from-kiro-cli finding. When the
// server is handed a marker path (as entrypoint.sh does via
// KIRO_CLI_READY_MARKER), /api/health returns 503 while the marker is absent (a
// failed/incomplete kiro-cli install) and 200 once it exists — reflecting
// web-terminal-kiro's core dependency with a cheap Stat, never launching kiro-cli. An
// empty marker path skips the gate, so out-of-container runs (tests, bare
// `go run`) keep pure-listener readiness.
func TestHealthEndpoint_reflectsKiroCliReadiness(t *testing.T) {
	marker := filepath.Join(t.TempDir(), ".kiro-cli-ready")

	newMux := func(markerPath string) *http.ServeMux {
		mux := http.NewServeMux()
		var ready atomic.Bool
		ready.Store(true)
		deps := &routeDeps{
			staticFS:        fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ok")}},
			ready:           &ready,
			workDir:         "",
			cmd:             []string{"/bin/cat"},
			kiroReadyMarker: markerPath,
		}
		mgr, err := registerRoutes(mux, deps)
		if err != nil {
			t.Fatalf("registerRoutes: %v", err)
		}
		t.Cleanup(mgr.Shutdown)
		return mux
	}
	status := func(mux *http.ServeMux) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", http.NoBody))
		return rec.Code
	}

	// Marker path set but file absent -> kiro-cli unavailable -> 503.
	if code := status(newMux(marker)); code != http.StatusServiceUnavailable {
		t.Errorf("marker absent: status = %d, want %d", code, http.StatusServiceUnavailable)
	}

	// Marker present -> ready -> 200.
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if code := status(newMux(marker)); code != http.StatusOK {
		t.Errorf("marker present: status = %d, want %d", code, http.StatusOK)
	}

	// Empty marker path -> gate disabled -> 200 even with no file on disk.
	if code := status(newMux("")); code != http.StatusOK {
		t.Errorf("marker gate disabled: status = %d, want %d", code, http.StatusOK)
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

// TestSSEStreamsThroughLoggingMiddleware is the regression guard for the tab
// status stream behind web-terminal-kiro's own middleware. webhttp.Logging wraps most
// requests in a webhttp.StatusRecorder; if the SSE path were wrapped by
// something opaque to streaming the engine's flush probe would fail and the
// stream would 500. It is instead in Logging's WithSkipPaths set (like /ws), so
// it flows through the streaming-transparent primitives. This drives
// /api/sessions/events through the full production middleware stack
// (buildHandler: Logging + Recoverer + SecurityHeaders + CrossOriginProtection)
// and asserts the stream opens (200 + text/event-stream) and flushes an event
// -- also proving the SecurityHeaders/Recoverer layers stay transparent to the
// SSE stream.
func TestSSEStreamsThroughLoggingMiddleware(t *testing.T) {
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

	srv := httptest.NewServer(buildHandler(mux, nil))
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
	t.Fatalf("SSE stream delivered no data through the logging middleware (scan err: %v)", sc.Err())
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
	for range createBurst {
		if post() == http.StatusOK {
			allowed++
		}
	}
	if allowed != createBurst {
		t.Errorf("allowed %d creates in the burst, want %d", allowed, createBurst)
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

// TestSecurityHeaders_presentOnNormalResponse pins the baseline response
// security headers that buildHandler layers on every response via
// webhttp.SecurityHeaders(). web-terminal-kiro sent NO security headers before the
// webhttp standardization, so this is the regression guard for the fleet
// baseline: X-Content-Type-Options nosniff, X-Frame-Options DENY, and
// Referrer-Policy strict-origin-when-cross-origin on a normal 200. It also pins
// the two deliberate choices -- X-Frame-Options is the DENY default because
// web-terminal-kiro is never embedded in a frame, and NO Content-Security-Policy is set,
// because a wrong CSP would silently break the terminal UI's fonts + WebSocket.
// Driven through the full production chain (buildHandler) so the assertion
// tracks what the server actually sends.
func TestSecurityHeaders_presentOnNormalResponse(t *testing.T) {
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

	rec := httptest.NewRecorder()
	buildHandler(mux, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/health: status = %d, want %d", rec.Code, http.StatusOK)
	}
	for _, tc := range []struct{ header, want string }{
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "strict-origin-when-cross-origin"},
	} {
		if got := rec.Header().Get(tc.header); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.header, got, tc.want)
		}
	}
	// No CSP by design: SecurityHeaders() is used without WithCSP so the
	// terminal UI's fonts and WebSocket are not gated by a policy web-terminal-kiro would
	// have to keep in lockstep with the vendored UI bundle.
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("Content-Security-Policy = %q, want unset (no CSP by design)", got)
	}
}

// TestWSRejectsCrossOrigin pins the WebSocket CSWSH guard. /ws is mounted via
// mgr.WebSocketHandler() with no WithAcceptOptions, so the engine relies on
// coder/websocket's secure-by-default same-origin check (nil AcceptOptions ->
// authenticateOrigin). http.NewCrossOriginProtection lets the GET upgrade
// through, so this same-origin check is the ONLY thing standing between a
// malicious page in the victim's browser and a kiro-cli PTY on localhost.
// Unlike /debug (TestDebugRoutesNotExposed) this posture had no regression
// guard: a future WithAcceptOptions{InsecureSkipVerify:true} would silently
// re-open cross-site WebSocket hijacking. This test fails if that happens.
func TestWSRejectsCrossOrigin(t *testing.T) {
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
	// A valid session id is required: WebSocketHandler returns 404 for an unknown
	// id BEFORE the upgrade, so the same-origin (CSWSH) guard only runs for an
	// existing session. Create one so the cross-origin handshake reaches
	// websocket.Accept (nil AcceptOptions) and is rejected with 403.
	id, err := mgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	srv := httptest.NewServer(buildHandler(mux, nil))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/ws?session="+id, http.NoBody)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") // gitleaks:allow (RFC 6455 example key)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("cross-origin /ws handshake: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin /ws handshake = %d, want 403 (CSWSH must be blocked; do not set InsecureSkipVerify)", resp.StatusCode)
	}
}

// TestWSAcceptsSameOrigin is the positive companion to TestWSRejectsCrossOrigin:
// a same-origin /ws handshake for a valid session must complete the upgrade
// (101 Switching Protocols). The cross-origin 403 test alone cannot distinguish
// "correctly rejects a foreign Origin" from "rejects every upgrade" -- a handler
// that 403'd unconditionally would still pass the negative test. This pins that
// the 403 is specifically the same-origin (CSWSH) check, not a blanket refusal.
func TestWSAcceptsSameOrigin(t *testing.T) {
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

	srv := httptest.NewServer(buildHandler(mux, nil))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/ws?session="+id, http.NoBody)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") // gitleaks:allow (RFC 6455 example key)
	req.Header.Set("Origin", srv.URL)                               // same origin as the test server
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("same-origin /ws handshake: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("same-origin /ws handshake = %d, want 101 (the CSWSH guard must ACCEPT a same-origin upgrade, else the cross-origin 403 test cannot tell the origin check from a blanket rejection)", resp.StatusCode)
	}
}

// TestClassifyStatus pins the kiro-cli OSC 9 -> latched-status mapping that
// drives the tab activity dots. It was an inline closure with no test, so a
// typo or an upstream wording drift in the magic strings would silently break
// the dots. The switch is case-sensitive, so a case mismatch must NOT latch.
func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		name      string
		msg       string
		want      string
		wantLatch bool
	}{
		{name: "response complete latches done", msg: "Response complete", want: terminal.StatusDone, wantLatch: true},
		{name: "permission required latches input", msg: "Permission required", want: terminal.StatusInput, wantLatch: true},
		{name: "unknown message is ignored", msg: "Working on it", want: "", wantLatch: false},
		{name: "empty message is ignored", msg: "", want: "", wantLatch: false},
		{name: "case mismatch is ignored", msg: "response complete", want: "", wantLatch: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, latch := classifyStatus(tc.msg)
			if got != tc.want || latch != tc.wantLatch {
				t.Errorf("classifyStatus(%q) = (%q, %v), want (%q, %v)", tc.msg, got, latch, tc.want, tc.wantLatch)
			}
		})
	}
}

// TestHealthEndpoint_reasonDistinguishesUnreadyCause pins the reason body of
// the two 503 paths, which TestHealthEndpoint_reflectsReadiness and
// TestHealthEndpoint_reflectsKiroCliReadiness leave unchecked: both assert only
// the status code, so the startup 503 and the kiro-cli-unavailable 503 are
// indistinguishable in the suite. The reason is the operator-facing diagnostic
// (documented as surfacing to docker ps / the monitoring probe), so a
// regression that emitted the wrong reason on the wrong branch -- or the same
// reason for both -- would lose the "wait for startup" vs "alert: kiro-cli
// broken" signal with no failing test. This pins each 503 branch to its reason.
func TestHealthEndpoint_reasonDistinguishesUnreadyCause(t *testing.T) {
	newMux := func(ready bool, markerPath string) *http.ServeMux {
		mux := http.NewServeMux()
		var r atomic.Bool
		r.Store(ready)
		deps := &routeDeps{
			staticFS:        fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ok")}},
			ready:           &r,
			workDir:         "",
			cmd:             []string{"/bin/cat"},
			kiroReadyMarker: markerPath,
		}
		mgr, err := registerRoutes(mux, deps)
		if err != nil {
			t.Fatalf("registerRoutes: %v", err)
		}
		t.Cleanup(mgr.Shutdown)
		return mux
	}
	body := func(mux *http.ServeMux) (int, string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", http.NoBody))
		return rec.Code, rec.Body.String()
	}

	// Not-ready (startup/shutdown): the ready gate short-circuits before the
	// marker check, so 503 with the startup reason regardless of the marker.
	code, b := body(newMux(false, filepath.Join(t.TempDir(), ".absent")))
	if code != http.StatusServiceUnavailable || !strings.Contains(b, "starting up or shutting down") {
		t.Errorf("not-ready: (status %d, body %q), want 503 with reason %q", code, b, "starting up or shutting down")
	}

	// Ready but kiro-cli marker absent: 503 with the kiro-cli reason, which must
	// differ from the startup reason so a probe can tell the two causes apart.
	code, b = body(newMux(true, filepath.Join(t.TempDir(), ".absent")))
	if code != http.StatusServiceUnavailable || !strings.Contains(b, "kiro-cli unavailable") {
		t.Errorf("kiro-cli-absent: (status %d, body %q), want 503 with reason %q", code, b, "kiro-cli unavailable")
	}
}
