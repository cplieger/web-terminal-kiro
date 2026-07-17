package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cplieger/toolbelt/v2"
	"github.com/cplieger/web-terminal-engine/v2/terminal"
	"github.com/cplieger/webhttp"
)

// TestDebugRoutesNotExposed pins the route surface of registerRoutes: the
// engine's terminal.MountAPI wires exactly its documented four routes (/ws,
// /api/sessions + subtree, /api/sessions/events), so no diagnostic or
// engine-internal path may ever answer on this unauthenticated surface. The
// /debug/* probes are canaries for that contract: the pinned engine exports no
// such routes today, and if any version ever grows one, MountAPI's
// release-noted route-set contract plus this test keep it from appearing here
// silently.
func TestDebugRoutesNotExposed(t *testing.T) {
	mux := http.NewServeMux()
	var ready atomic.Bool
	deps := &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, _, err := registerRoutes(mux, deps)
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
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, _, err := registerRoutes(mux, deps)
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
			staticFS:        fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
			ready:           &ready,
			workDir:         "",
			cmd:             []string{"/bin/cat"},
			kiroReadyMarker: markerPath,
		}
		mgr, _, err := registerRoutes(mux, deps)
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

// testIndexHTML is the minimal index fixture route tests embed: it carries one
// inline <script> (the importmap stand-in) so buildCSPPolicy — which fails
// loud on a script-less index.html — accepts it, mirroring the real page.
const testIndexHTML = `<!doctype html><script type="importmap">{}</script>`

// TestKiroCacheControl pins the two-branch cache POLICY handed to
// webhttp.StaticHandler (the ETag/gzip mechanism now lives in webhttp and is
// tested there): assets under vendor/fonts/ are immutable for 30 days while
// everything else is no-cache + must-revalidate so deploys take effect at
// once. Paths arrive normalized (no leading slash; "index.html" for "/"), and
// the fonts prefix's trailing slash is load-bearing -- "vendor/fonts-list.json"
// must NOT be treated as a font.
func TestKiroCacheControl(t *testing.T) {
	cases := []struct {
		name      string
		assetPath string
		wantCache string
	}{
		{name: "font is immutable", assetPath: "vendor/fonts/iosevka.woff2", wantCache: "public, max-age=2592000, immutable"},
		{name: "nested font is immutable", assetPath: "vendor/fonts/sub/x.woff2", wantCache: "public, max-age=2592000, immutable"},
		{name: "html is no-cache", assetPath: "index.html", wantCache: "no-cache, must-revalidate"},
		{name: "js bundle is no-cache", assetPath: "app.js", wantCache: "no-cache, must-revalidate"},
		{name: "vendor non-font prefix is no-cache", assetPath: "vendor/fonts-list.json", wantCache: "no-cache, must-revalidate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kiroCacheControl(tc.assetPath); got != tc.wantCache {
				t.Errorf("kiroCacheControl(%q) = %q, want %q", tc.assetPath, got, tc.wantCache)
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
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, _, err := registerRoutes(mux, deps)
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
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, csp, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	id, err := mgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	srv := httptest.NewServer(buildHandler(mux, nil, csp))
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

// sessionCreateBurst pins the burst of webhttp.SessionCreateRateLimit as THIS
// app's documented contract (six creates, then 429). A deliberate tuning
// change in the shared preset fails this test loudly so the app's docs and
// expectations are updated consciously rather than drifting silently.
const sessionCreateBurst = 6

// TestCreateRateLimit pins the create throttle: a burst of POST /api/sessions is
// allowed, then further creates are 429'd, while GET (list) is never limited. It
// exercises the shared preset exactly as registerRoutes wires it, with a stub
// next handler so it does not fork real kiro-cli processes.
func TestCreateRateLimit(t *testing.T) {
	var restHit atomic.Bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		restHit.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	h := webhttp.SessionCreateRateLimit("/api/sessions")(next)
	post := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", http.NoBody))
		return rec.Code
	}

	allowed := 0
	for range sessionCreateBurst {
		if post() == http.StatusOK {
			allowed++
		}
	}
	if allowed != sessionCreateBurst {
		t.Errorf("allowed %d creates in the burst, want %d", allowed, sessionCreateBurst)
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
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, csp, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)

	rec := httptest.NewRecorder()
	buildHandler(mux, nil, csp).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", http.NoBody))

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
	// The CSP is hash-pinned from the embedded index.html (buildCSPPolicy via
	// webhttp.InlineScriptHashes): script-src carries 'self' plus at least one
	// sha256 token and never 'unsafe-inline'; style-src keeps 'unsafe-inline'
	// for the renderer's per-cell styles. This closed the family-drift gap
	// where web-terminal-kiro served the same embedded-static + inline-importmap
	// pattern as web-terminal-server with no CSP at all.
	servedCSP := rec.Header().Get("Content-Security-Policy")
	if servedCSP == "" {
		t.Fatal("Content-Security-Policy is unset; the hash-pinned policy must be served on every response")
	}
	if !strings.Contains(servedCSP, "script-src 'self' 'sha256-") {
		t.Errorf("CSP script-src = %q, want 'self' plus a pinned sha256 token", servedCSP)
	}
	if strings.Contains(servedCSP, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP = %q, want script-src without 'unsafe-inline'", servedCSP)
	}
	for _, want := range []string{
		"default-src 'self'", "style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:", "connect-src 'self'", "frame-ancestors 'none'",
	} {
		if !strings.Contains(servedCSP, want) {
			t.Errorf("CSP = %q, want it to contain %q", servedCSP, want)
		}
	}
}

// TestCSPScriptHashesMatchEmbeddedInlineScripts is the anti-drift guard for
// the script-src hardening, ported from web-terminal-server: it independently
// re-extracts every inline <script> in the REAL embedded index.html with a
// regexp (a different implementation from webhttp's byte scanner, so agreement
// is a genuine cross-check) and asserts the sha256 hash of each appears in the
// CSP buildCSPPolicy assembles. Hashes are computed from the embed, never
// hardcoded, so the test tracks index.html automatically.
func TestCSPScriptHashesMatchEmbeddedInlineScripts(t *testing.T) {
	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded static/index.html: %v", err)
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	csp, err := buildCSPPolicy(sub)
	if err != nil {
		t.Fatalf("buildCSPPolicy: %v", err)
	}

	scriptRE := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	srcRE := regexp.MustCompile(`(?i)(^|[\s/])src\s*=`)

	found := 0
	for _, m := range scriptRE.FindAllSubmatch(indexHTML, -1) {
		if srcRE.Match(m[1]) {
			continue // external script (/app.js), covered by 'self'
		}
		found++
		sum := sha256.Sum256(m[2])
		token := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(csp, token) {
			t.Errorf("CSP is missing the hash for an inline script.\ncontent=%q\nwant token %s\nCSP: %s", m[2], token, csp)
		}
	}
	if found < 1 {
		t.Fatalf("oracle found %d inline scripts in index.html, want >= 1 (the importmap); the regexp or the file changed", found)
	}
}

// TestBuildCSPPolicyFailsLoud pins the fail-loud contract: buildCSPPolicy
// returns an error (never a silent 'unsafe-inline' degrade) when the static FS
// is nil, index.html is missing, or index.html holds no inline <script>. A
// production build always embeds index.html with its inline importmap, so any
// of these means a malformed build that must abort startup.
func TestBuildCSPPolicyFailsLoud(t *testing.T) {
	cases := []struct {
		name string
		fsys fs.FS
	}{
		{"nil FS", nil},
		{"missing index.html", fstest.MapFS{}},
		{"only external scripts", fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<html><body><script src="/app.js"></script></body></html>`)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildCSPPolicy(tc.fsys); err == nil {
				t.Errorf("buildCSPPolicy(%s) = nil error, want a fail-loud error", tc.name)
			}
		})
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
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, csp, err := registerRoutes(mux, deps)
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

	srv := httptest.NewServer(buildHandler(mux, nil, csp))
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
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, csp, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	id, err := mgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	srv := httptest.NewServer(buildHandler(mux, nil, csp))
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
			staticFS:        fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
			ready:           &r,
			workDir:         "",
			cmd:             []string{"/bin/cat"},
			kiroReadyMarker: markerPath,
		}
		mgr, _, err := registerRoutes(mux, deps)
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

// newToolsDeps builds routeDeps with a real toolbelt engine on temp dirs
// (no catalog: search degrades, installs would fail — irrelevant here,
// these tests exercise the HTTP boundary, not installs).
func newToolsDeps(t *testing.T) *routeDeps {
	t.Helper()
	dir := t.TempDir()
	eng, err := toolbelt.New(&toolbelt.Config{
		ConfigDir: dir,
		ToolsDir:  filepath.Join(dir, "tools"),
	})
	if err != nil {
		t.Fatalf("toolbelt.New: %v", err)
	}
	t.Cleanup(eng.Close)
	var ready atomic.Bool
	ready.Store(true)
	return &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
		tools:    eng,
	}
}

// TestToolsAPI_LoopbackOnly pins the tools API's only boundary on this
// unauthenticated port: the SOCKET PEER must be loopback. A remote peer
// gets 403 regardless of headers (forwarded headers are client-controlled
// and deliberately ignored); the in-container consumer (curl localhost)
// passes and reads the inventory.
func TestToolsAPI_LoopbackOnly(t *testing.T) {
	mux := http.NewServeMux()
	deps := newToolsDeps(t)
	mgr, _, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)

	// Remote peer: refused, even claiming loopback via forwarded headers.
	req := httptest.NewRequest(http.MethodGet, "/api/tools", http.NoBody)
	req.RemoteAddr = "203.0.113.9:44321"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote peer: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	// Loopback peer: served. The fresh engine has an empty manifest, so
	// the inventory decodes with zero tools.
	req = httptest.NewRequest(http.MethodGet, "/api/tools", http.NoBody)
	req.RemoteAddr = "127.0.0.1:55555"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback peer: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var inv struct {
		Tools []struct{} `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatalf("inventory decode: %v", err)
	}
	if len(inv.Tools) != 0 {
		t.Fatalf("fresh inventory = %d tools, want 0", len(inv.Tools))
	}

	// IPv6 loopback passes too.
	req = httptest.NewRequest(http.MethodGet, "/api/tools", http.NoBody)
	req.RemoteAddr = "[::1]:55555"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ipv6 loopback peer: status = %d, want 200", rec.Code)
	}
}

// TestToolsAPI_AbsentWithoutEngine pins the no-engine shape (bare `go run`
// / tests outside the container): /api/tools is simply not a registered
// pattern, falling through to the static catch-all.
func TestToolsAPI_AbsentWithoutEngine(t *testing.T) {
	mux := http.NewServeMux()
	var ready atomic.Bool
	deps := &routeDeps{
		staticFS: fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte(testIndexHTML)}},
		ready:    &ready,
		workDir:  "",
		cmd:      []string{"/bin/cat"},
	}
	mgr, _, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	if _, pat := mux.Handler(httptest.NewRequest(http.MethodGet, "/api/tools", http.NoBody)); pat == "/api/tools" {
		t.Fatal("/api/tools registered without a tools engine")
	}
}

// TestSessionCreateGate_ToolsSyncing pins the boot-convergence session
// gate: while the tools reconcile runs, POST /api/sessions answers 503
// ("tools installing") so the first kiro-cli never spawns before the
// manifest's tools are on PATH; once the gate lifts, creation flows
// through to the engine (and its rate limit) again. Health and the tools
// API stay reachable throughout — that is the bind-first point.
func TestSessionCreateGate_ToolsSyncing(t *testing.T) {
	mux := http.NewServeMux()
	deps := newToolsDeps(t)
	var syncing atomic.Bool
	syncing.Store(true)
	deps.toolsSyncing = syncing.Load
	deps.toolsState = func() string { return "syncing" }
	mgr, _, err := registerRoutes(mux, deps)
	if err != nil {
		t.Fatalf("registerRoutes: %v", err)
	}
	t.Cleanup(mgr.Shutdown)

	create := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := create(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create during sync: status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), "tools installing") {
		t.Fatalf("create during sync: body %q missing the reason", rec.Body.String())
	}

	// Health stays reachable and reports the informational tools state.
	hreq := httptest.NewRequest(http.MethodGet, "/api/health", http.NoBody)
	hrec := httptest.NewRecorder()
	mux.ServeHTTP(hrec, hreq)
	if hrec.Code != http.StatusOK || !strings.Contains(hrec.Body.String(), `"tools":"syncing"`) {
		t.Fatalf("health during sync = %d %s, want 200 with tools:syncing", hrec.Code, hrec.Body.String())
	}

	// Gate lifts: the composed gate passes requests through to the inner
	// chain again. Asserted against a stub inner handler rather than the
	// real create endpoint — spawning an actual PTY session here would
	// leak its logging goroutines into later tests that capture
	// slog.Default (the client-ip threading test), which the race
	// detector rightly flags.
	syncing.Store(false)
	inner := 0
	gate := composeGate(func(next http.Handler) http.Handler { return next }, syncing.Load)
	gated := gate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		inner++
		w.WriteHeader(http.StatusCreated)
	}))
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", http.NoBody))
	if rec.Code != http.StatusCreated || inner != 1 {
		t.Fatalf("create after sync: status %d inner %d, want pass-through", rec.Code, inner)
	}
	syncing.Store(true)
	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable || inner != 1 {
		t.Fatalf("re-gated create: status %d inner %d, want 503 and no inner call", rec.Code, inner)
	}
}
