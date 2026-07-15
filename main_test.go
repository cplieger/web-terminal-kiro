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
