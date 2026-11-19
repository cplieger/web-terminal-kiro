package main

import (
	"maps"
	"testing"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/httpserver/v2/httpservertest"
	"github.com/cplieger/toolbelt/v3/httpapi"
)

// The package writes exactly codeSpecs plus the httpserver codes it answers
// itself, and every code the listener answers, the tools API's included,
// reaches the client with a remediation.
func TestCodeSpecs_matchSource(t *testing.T) {
	written := maps.Clone(codeSpecs)
	lib := httpserver.CodeSpecs()
	written["method_not_allowed"] = lib["method_not_allowed"]
	httpservertest.CheckCodeSpecs(t, ".", written)
	all := maps.Clone(codeSpecs)
	maps.Copy(all, httpapi.CodeSpecs())
	for _, code := range []httpserver.ErrorCode{"host_not_allowed", "rate_limited", "too_many_auth_failures", "cross_origin_refused", "loopback_only", "method_not_allowed", "internal_error"} {
		all[code] = httpserver.CodeSpec{}
	}
	for code := range all {
		if spec, ok := catalog.Lookup(string(code)); !ok || spec.Remediation == "" {
			t.Errorf("catalog has no remediation for %q", code)
		}
	}
}
