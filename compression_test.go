package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/go/httpserver"
)

// TestBuildHandlerGzipsJSON pins the compression layer of the production
// stack: a JSON body past the threshold is gzipped for a client offering gzip
// (RFC 9110 section 8.4.1.3) and sent as identity to one that does not, both
// with Vary: Accept-Encoding (section 12.5.5) so a cache keeps them apart.
func TestBuildHandlerGzipsJSON(t *testing.T) {
	type listing struct {
		Names []string `json:"names"`
	}
	payload := listing{Names: strings.Fields(strings.Repeat("session-title ", 200))}
	want, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, _ *http.Request) {
		httpserver.WriteJSON(w, payload)
	})
	h := buildHandler(mux, nil, "default-src 'self'", nil)

	t.Run("gzip-offering client gets a gzip body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/probe", http.NoBody)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip for a %d-byte JSON body", ce, len(want))
		}
		if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
			t.Errorf("Vary = %q, want it to name Accept-Encoding", v)
		}
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if strings.TrimSpace(string(got)) != string(want) {
			t.Errorf("decompressed body = %q, want %q", got, want)
		}
	})

	t.Run("client without Accept-Encoding gets identity", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe", http.NoBody))

		if ce := rec.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("Content-Encoding = %q, want none for a client that offers no coding", ce)
		}
		if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
			t.Errorf("Vary = %q, want it to name Accept-Encoding on the identity response too", v)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != string(want) {
			t.Errorf("body = %q, want %q", got, want)
		}
	})
}
