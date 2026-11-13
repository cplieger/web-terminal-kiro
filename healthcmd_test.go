package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// serveOnLoopback serves h on 127.0.0.1 and returns the ":port" listen address
// the health command is given, as with the image's LISTEN_ADDR=:9848.
func serveOnLoopback(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return ":" + port
}

// TestHealthCommand_readsTheServedVerdict runs the health command against the
// app's own handler chain, with ALLOWED_HOSTS naming only an outside name: the
// probe's loopback request must still be admitted, and the exit code must follow
// the /healthz verdict.
func TestHealthCommand_readsTheServedVerdict(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", "webterm.example.com")
	for _, tc := range []struct {
		name  string
		ready bool
		want  int
	}{
		{"ready", true, 0},
		{"not ready", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, _, csp := mustRegisterRoutes(t, newTestDeps(tc.ready))
			addr := serveOnLoopback(t, buildHandler(mux, nil, csp, parseAllowedHosts()))

			var out bytes.Buffer
			if got := healthCommand(&out, addr); got != tc.want {
				t.Fatalf("healthCommand(%q) = %d, want %d (output %q)", addr, got, tc.want, out.String())
			}
			if tc.want != 0 && !strings.Contains(out.String(), "503") {
				t.Errorf("output = %q, want the failure to name the 503", out.String())
			}
		})
	}
}

// TestHealthCommand_probesTheNamedAddress serves on 127.0.0.2 only, so a health
// command that probed 127.0.0.1 for an address naming one host would be refused.
func TestHealthCommand_probesTheNamedAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux routes all of 127.0.0.0/8 to loopback")
	}
	t.Setenv("ALLOWED_HOSTS", "webterm.example.com")
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	mux, _, csp := mustRegisterRoutes(t, newTestDeps(true))
	srv := httptest.NewUnstartedServer(buildHandler(mux, nil, csp, parseAllowedHosts()))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	addr := ln.Addr().String()
	var out bytes.Buffer
	if got := healthCommand(&out, addr); got != 0 {
		t.Fatalf("healthCommand(%q) = %d, want 0 (output %q)", addr, got, out.String())
	}
}

// TestHealthCommand_badListenAddr pins that an address the probe cannot turn
// into a URL fails the check with a reason instead of probing a guess.
func TestHealthCommand_badListenAddr(t *testing.T) {
	var out bytes.Buffer
	if got := healthCommand(&out, "no-port"); got != 1 {
		t.Fatalf("healthCommand(%q) = %d, want 1", "no-port", got)
	}
	if !strings.Contains(out.String(), "LISTEN_ADDR") {
		t.Errorf("output = %q, want it to name LISTEN_ADDR", out.String())
	}
}

// TestListenAddr_default pins that the health command and the server read the
// same default, so an unset LISTEN_ADDR is probed where the server binds.
func TestListenAddr_default(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "")
	if got := listenAddr(); got != ":9848" {
		t.Errorf("listenAddr() = %q, want :9848", got)
	}
	t.Setenv("LISTEN_ADDR", "127.0.0.1:7000")
	if got := listenAddr(); got != "127.0.0.1:7000" {
		t.Errorf("listenAddr() = %q, want 127.0.0.1:7000", got)
	}
}

// TestHealthProbe_singleAddressBindAdmitted pins that the host gate admits a
// request whose peer and Host are the non-loopback address the server is bound
// to, which ALLOWED_HOSTS does not list, and refuses another peer naming it.
func TestHealthProbe_singleAddressBindAdmitted(t *testing.T) {
	t.Setenv("ALLOWED_HOSTS", "webterm.example.com")
	mux, _, csp := mustRegisterRoutes(t, newTestDeps(true))
	h := buildHandler(mux, nil, csp, parseAllowedHosts())

	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.5"), Port: 9848}
	for _, tc := range []struct {
		name, remoteAddr string
		want             int
	}{
		{"probe from the bound address", "192.0.2.5:54321", http.StatusOK},
		{"another peer naming the bound address", "192.0.2.9:54321", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://192.0.2.5:9848"+healthPath, http.NoBody)
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("GET %s from %s = %d, want %d", healthPath, tc.remoteAddr, rec.Code, tc.want)
			}
		})
	}
}
