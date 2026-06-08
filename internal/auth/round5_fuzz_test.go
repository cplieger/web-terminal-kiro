package auth

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/cplieger/vibecli/internal/api"
)

// FuzzClientIPPortStrip verifies clientIP correctly extracts the host from
// host:port pairs and returns raw input when SplitHostPort fails.
// Bug class: IP spoofing in audit logs via malformed RemoteAddr values.
func FuzzClientIPPortStrip(f *testing.F) {
	f.Add("192.168.1.1:8080")
	f.Add("[::1]:443")
	f.Add("no-port")
	f.Add("host:0")
	f.Add("[fe80::1%eth0]:9848")
	f.Add(":80")

	f.Fuzz(func(t *testing.T, addr string) {
		r := &http.Request{RemoteAddr: addr}
		got := clientIP(r)

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			if got != addr {
				t.Errorf("clientIP(%q)=%q, want raw addr on SplitHostPort error", addr, got)
			}
		} else {
			if got != host {
				t.Errorf("clientIP(%q)=%q, want host=%q", addr, got, host)
			}
		}
	})
}

// FuzzStderrAttrSanitization checks stderrAttr returns nil for empty/whitespace
// input and sanitized output otherwise.
// Bug class: log injection / ANSI escape persistence in structured logs.
func FuzzStderrAttrSanitization(f *testing.F) {
	f.Add("\x1b[31mred\x1b[0m")
	f.Add("   \n\t  ")
	f.Add("normal stderr output")
	f.Add("\u200Bhidden")
	f.Add("")
	f.Add("line1\nline2")

	f.Fuzz(func(t *testing.T, input string) {
		buf := bytes.NewBufferString(input)
		result := stderrAttr(buf)

		expected := api.SanitizeOutput(strings.TrimSpace(input))
		if expected == "" {
			if result != nil {
				t.Errorf("stderrAttr(%q) = %v, want nil for empty sanitized input", input, result)
			}
		} else {
			if result == nil {
				t.Errorf("stderrAttr(%q) = nil, want non-nil for sanitized=%q", input, expected)
			} else if result[1].(string) != expected {
				t.Errorf("stderrAttr(%q)[1] = %q, want %q", input, result[1], expected)
			}
		}
	})
}

// FuzzClassifyCLIErrorExhaustive checks classifyCLIError never returns nil and
// always assigns a valid Kind from the exhaustive set.
// Bug class: nil-pointer dereference or wrong HTTP status from unhandled error types.
func FuzzClassifyCLIErrorExhaustive(f *testing.F) {
	f.Add(true, true)
	f.Add(true, false)
	f.Add(false, true)
	f.Add(false, false)
	f.Add(false, true)
	f.Add(true, true)

	f.Fuzz(func(t *testing.T, deadlineExceeded, notFound bool) {
		ctx := context.Background()
		if deadlineExceeded {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 0)
			defer cancel()
			// Force deadline to expire
			<-ctx.Done()
		}

		var err error
		switch {
		case notFound:
			err = exec.ErrNotFound
		default:
			err = errors.New("generic failure")
		}

		result := classifyCLIError(ctx, err)
		if result == nil {
			t.Fatal("classifyCLIError returned nil")
		}
		if result.Kind < CLIErrTimeout || result.Kind > CLIErrFailed {
			t.Errorf("classifyCLIError returned invalid Kind=%d", result.Kind)
		}

		// Verify classification logic
		if deadlineExceeded {
			if result.Kind != CLIErrTimeout {
				t.Errorf("deadline exceeded but Kind=%d, want CLIErrTimeout", result.Kind)
			}
		} else if notFound {
			if result.Kind != CLIErrNotFound {
				t.Errorf("ErrNotFound but Kind=%d, want CLIErrNotFound", result.Kind)
			}
		} else {
			if result.Kind != CLIErrFailed {
				t.Errorf("generic error but Kind=%d, want CLIErrFailed", result.Kind)
			}
		}
	})
}
