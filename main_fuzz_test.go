package main

import (
	"net"
	"strings"
	"testing"
)

// isPlainHostToken reports whether s is an ASCII token free of ':', '[' and
// ']', the shape for which the port/case/trailing-dot spellings of the same
// host must canonicalize identically. Excluding ':'/'['/']' keeps the variant
// construction unambiguous (an embedded colon changes how net.SplitHostPort
// parses the variant, not how canonicalHost treats the host itself), and
// excluding non-ASCII sidesteps Unicode case-folding asymmetries
// (ToLower(ToUpper("ſ")) != "ſ") that no browser-sent Host header contains.
func isPlainHostToken(s string) bool {
	for i := range len(s) {
		c := s[i]
		if c >= 0x80 || c == ':' || c == '[' || c == ']' {
			return false
		}
	}
	return true
}

// FuzzCanonicalHost fuzzes the Host-header canonicalizer behind the
// KWEB_ALLOWED_HOSTS DNS-rebinding gate (hostAllowlist). r.Host is
// attacker-controlled, so the canonicalizer must hold its contract on
// arbitrary bytes, not only on well-formed headers. Three invariants:
//
//  1. lowercase: the returned string is the exact map key parseAllowedHosts
//     stores, so it must always be lowercase — a mixed-case return could
//     never match a stored entry, opening a config-vs-request asymmetry.
//  2. IP-oracle: any input net.ParseIP accepts must collapse to ip.String(),
//     the property that makes ::1 and 0:0:0:0:0:0:0:1 compare equal.
//  3. spelling-collapse (metamorphic): for a plain hostname token, the
//     :port, UPPERCASE, and single-trailing-dot spellings a browser can
//     legitimately send must all canonicalize to the bare form's key.
func FuzzCanonicalHost(f *testing.F) {
	for _, s := range []string{
		"localhost", "LOCALHOST:9848", "127.0.0.1:9848", "[::1]:9848",
		"0:0:0:0:0:0:0:1", "webterm.example.com.", "WEBTERM.Example.COM.:1234",
		"", ":9848", "attacker.evil:9848", "[::ffff:127.0.0.1]:80",
		"a:b:c", "[[::1]]", "example.com..", "192.168.1.5",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, host string) {
		got := canonicalHost(host)
		if got != strings.ToLower(got) {
			t.Errorf("canonicalHost(%q) = %q; the allowlist map key must be lowercase", host, got)
		}
		if ip := net.ParseIP(host); ip != nil {
			if want := ip.String(); got != want {
				t.Errorf("canonicalHost(%q) = %q, want the canonical IP form %q", host, got, want)
			}
		}
		if !isPlainHostToken(host) {
			return
		}
		if v := canonicalHost(host + ":9848"); v != got {
			t.Errorf("canonicalHost(%q+\":9848\") = %q, want the bare form's key %q (a port must not change the key)", host, v, got)
		}
		if v := canonicalHost(strings.ToUpper(host)); v != got {
			t.Errorf("canonicalHost(ToUpper(%q)) = %q, want %q (case must not change the key)", host, v, got)
		}
		if !strings.HasSuffix(host, ".") {
			if v := canonicalHost(host + "."); v != got {
				t.Errorf("canonicalHost(%q+\".\") = %q, want %q (one trailing FQDN dot must not change the key)", host, v, got)
			}
		}
	})
}
