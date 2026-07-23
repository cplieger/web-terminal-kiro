package main

import (
	"strings"
	"testing"
)

// TestIncompatibility pins the two directional floor checks and the named
// remediation in each failure message: the server must meet the client's
// minimum server revision, and the client must meet the server's minimum
// client revision. Values are exercised around the current 4/3 floors so a
// future floor raise re-runs meaningful boundaries.
func TestIncompatibility(t *testing.T) {
	cases := []struct {
		name                                                   string
		serverRev, serverMinClient, clientRev, clientMinServer int
		wantSubstr                                             string // "" = compatible
	}{
		{name: "current pairing compatible", serverRev: 4, serverMinClient: 3, clientRev: 4, clientMinServer: 3},
		{name: "skew within floors compatible", serverRev: 5, serverMinClient: 3, clientRev: 4, clientMinServer: 4},
		{name: "server below client floor", serverRev: 3, serverMinClient: 3, clientRev: 5, clientMinServer: 4,
			wantSubstr: "bump go.mod"},
		{name: "client below server floor", serverRev: 5, serverMinClient: 5, clientRev: 4, clientMinServer: 3,
			wantSubstr: "bump the Dockerfile ARG"},
		{name: "equal at both floors compatible", serverRev: 3, serverMinClient: 3, clientRev: 3, clientMinServer: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := incompatibility(tc.serverRev, tc.serverMinClient, tc.clientRev, tc.clientMinServer)
			if tc.wantSubstr == "" {
				if got != "" {
					t.Errorf("incompatibility(%d,%d,%d,%d) = %q, want compatible",
						tc.serverRev, tc.serverMinClient, tc.clientRev, tc.clientMinServer, got)
				}
				return
			}
			if got == "" || !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("incompatibility(%d,%d,%d,%d) = %q, want reason containing %q",
					tc.serverRev, tc.serverMinClient, tc.clientRev, tc.clientMinServer, got, tc.wantSubstr)
			}
		})
	}
}
