package main

import (
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// trustedContains reports whether ip is inside any of the parsed trusted nets.
func trustedContains(nets []*net.IPNet, ip string) bool {
	parsed := net.ParseIP(ip)
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// mustCIDR parses a CIDR for test setup, failing the test on a bad literal.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("net.ParseCIDR(%q): %v", s, err)
	}
	return n
}

// TestParseTrustedProxies pins the TRUSTED_PROXIES parsing that feeds
// webhttp.WithClientIP via the shared webhttp.ParseCIDRs helper. Three
// contracts: an unset/blank var yields nil (so ClientIP ignores X-Forwarded-For
// and logs the spoof-proof socket peer — the directly-exposed default), a valid
// CIDR + bare-IP mix parses into containment-correct nets, and a malformed entry
// is warned (named) and skipped while the valid subset is kept — startup is
// never aborted and never falls open. The malformed case mutates the
// process-global default logger, so the subtests run serially (no t.Parallel).
func TestParseTrustedProxies(t *testing.T) {
	t.Run("unset/empty yields nil (socket-peer default)", func(t *testing.T) {
		t.Setenv("TRUSTED_PROXIES", "")
		if got := parseTrustedProxies(); got != nil {
			t.Errorf("parseTrustedProxies = %v, want nil when TRUSTED_PROXIES is empty", got)
		}
	})

	t.Run("whitespace-only yields nil", func(t *testing.T) {
		t.Setenv("TRUSTED_PROXIES", "  ,  , ")
		if got := parseTrustedProxies(); got != nil {
			t.Errorf("parseTrustedProxies = %v, want nil for a blank list", got)
		}
	})

	t.Run("valid CIDR and bare-IP mix parsed", func(t *testing.T) {
		t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.5 , ::1")
		nets := parseTrustedProxies()
		if len(nets) != 3 {
			t.Fatalf("parseTrustedProxies len = %d, want 3 (%v)", len(nets), nets)
		}
		// The CIDR contains its range; the bare IP became a single-host net.
		for _, c := range []struct {
			ip   string
			want bool
		}{
			{"10.255.0.1", true},   // inside 10.0.0.0/8
			{"192.168.1.5", true},  // the bare host itself
			{"192.168.1.6", false}, // a neighbor of the bare host is NOT trusted
			{"172.16.0.1", false},  // outside every entry
			{"::1", true},          // the bare IPv6 host itself
		} {
			if got := trustedContains(nets, c.ip); got != c.want {
				t.Errorf("trusted contains %s = %v, want %v", c.ip, got, c.want)
			}
		}
	})

	t.Run("malformed entries are warned and skipped, valid subset kept", func(t *testing.T) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, not-an-ip, 999.999.999.999")
		nets := parseTrustedProxies()

		// Startup is not aborted; only the one valid CIDR is kept.
		if len(nets) != 1 {
			t.Fatalf("parseTrustedProxies len = %d, want 1 (only the valid CIDR kept)", len(nets))
		}
		if !trustedContains(nets, "10.1.2.3") {
			t.Error("kept net does not contain 10.1.2.3; want the 10.0.0.0/8 entry retained")
		}
		// A Warn line naming each malformed entry was emitted.
		log := buf.String()
		if log == "" {
			t.Fatal("no slog output; want a Warn naming the malformed entries")
		}
		for _, bad := range []string{"not-an-ip", "999.999.999.999"} {
			if !strings.Contains(log, bad) {
				t.Errorf("warn log %q does not name malformed entry %q", log, bad)
			}
		}
	})
}

// TestBuildHandlerClientIPThreading proves the trusted-proxy set is threaded
// into webhttp.WithClientIP and drives the access log's client_ip field
// end-to-end through the production middleware chain (buildHandler). Two
// contracts: with NO trusted proxies (unset default) the logged client_ip is the
// unspoofable socket peer and a client-supplied X-Forwarded-For is ignored
// (spoof-safe); with the socket peer inside the trusted set, client_ip resolves
// to the real client from the trusted XFF. httptest.NewRequest gives a fixed
// RemoteAddr of 192.0.2.1:1234, so the peer host is 192.0.2.1. This mutates the
// process-global default logger, so it runs serially (no t.Parallel).
func TestBuildHandlerClientIPThreading(t *testing.T) {
	const (
		peerIP = "192.0.2.1"   // httptest.NewRequest default RemoteAddr host
		xffIP  = "203.0.113.7" // the "real" client behind a proxy
	)
	cases := []struct {
		name    string
		trusted []*net.IPNet
		wantIP  string
	}{
		{
			name:    "unset trusts nothing: client_ip is the socket peer, XFF ignored",
			trusted: nil,
			wantIP:  peerIP,
		},
		{
			name:    "trusted peer resolves the real client from X-Forwarded-For",
			trusted: []*net.IPNet{mustCIDR(t, "192.0.2.0/24")},
			wantIP:  xffIP,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			// Capture the access line: buildHandler wires WithLogger(slog.Default())
			// at construction, so set the default logger before building it.
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			mux := http.NewServeMux()
			mux.HandleFunc("/probe", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodGet, "/probe", http.NoBody)
			req.Header.Set("X-Forwarded-For", xffIP)
			// Synchronous ServeHTTP: the deferred access-log line fires before it
			// returns, so buf is populated with no goroutine race.
			buildHandler(mux, tc.trusted).ServeHTTP(httptest.NewRecorder(), req)

			want := "client_ip=" + tc.wantIP
			if !strings.Contains(buf.String(), want) {
				t.Errorf("access log = %q, want it to contain %q", buf.String(), want)
			}
		})
	}
}
