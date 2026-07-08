package main

import "testing"

// TestIsExposedBind pins the security guard behind the "unauthenticated shell
// on a non-loopback address" warning: the wildcard forms and any routable IP
// are exposed, while an explicit loopback bind (127.0.0.0/8, ::1, or the
// "localhost" name) is safe, and an unparseable addr is not flagged. A
// regression dropping the ip==nil disjunct (which covers the empty-host
// wildcard and a hostname) would stop warning operators they are exposing a
// filesystem-access shell.
func TestIsExposedBind(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{name: "wildcard empty host is exposed", addr: ":9848", want: true},
		{name: "0.0.0.0 wildcard is exposed", addr: "0.0.0.0:9848", want: true},
		{name: "ipv6 wildcard is exposed", addr: "[::]:9848", want: true},
		{name: "routable LAN ip is exposed", addr: "192.168.1.5:9848", want: true},
		{name: "public ip is exposed", addr: "203.0.113.7:9848", want: true},
		{name: "hostname is exposed", addr: "myhost:9848", want: true},
		{name: "ipv4 loopback is safe", addr: "127.0.0.1:9848", want: false},
		{name: "ipv4 loopback subnet is safe", addr: "127.0.0.2:9848", want: false},
		{name: "ipv6 loopback is safe", addr: "[::1]:9848", want: false},
		{name: "localhost name is safe", addr: "localhost:9848", want: false},
		{name: "unparseable addr is not flagged", addr: "9848", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExposedBind(tc.addr); got != tc.want {
				t.Errorf("isExposedBind(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestEnvOr_emptyEnvFallsBackToDefault pins envOr with hardcoded expectations:
// a set, non-empty variable wins; an unset variable falls back; and a variable
// set to the EMPTY STRING is treated as unset and also falls back. The
// empty-string-as-unset case is the deliberate contract worth pinning: envOr's
// `v != ""` guard treats "" the same as unset, which os.Getenv alone does not
// express.
func TestEnvOr_emptyEnvFallsBackToDefault(t *testing.T) {
	const key = "WEB_TERMINAL_KIRO_ENVOR_TEST_KEY"
	cases := []struct {
		name  string
		value string
		want  string
		set   bool
	}{
		{name: "set and non-empty returns the value", value: "chosen", set: true, want: "chosen"},
		{name: "unset returns the fallback", set: false, want: "fallback"},
		{name: "set to empty string is treated as unset", value: "", set: true, want: "fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.value)
			}
			if got := envOr(key, "fallback"); got != tc.want {
				t.Errorf("envOr(%q, %q) = %q, want %q", key, "fallback", got, tc.want)
			}
		})
	}
}
