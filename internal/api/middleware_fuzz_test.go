package api

import (
	"strings"
	"testing"
)

func FuzzValidRequestID(f *testing.F) {
	f.Add("abc-123_XYZ")
	f.Add("")
	f.Add("a]b\nc")
	f.Add(strings.Repeat("a", 65))
	f.Fuzz(func(t *testing.T, s string) {
		ok := validRequestID(s)
		if ok {
			if len(s) < 1 || len(s) > 64 {
				t.Errorf("validRequestID accepted out-of-range length: %d", len(s))
			}
			for _, r := range s {
				if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
					t.Errorf("validRequestID accepted invalid rune %q in %q", r, s)
				}
			}
		}
	})
}

// FuzzRequestIDOrNew verifies the generator's output is always in the
// valid set: requestIDOrNew must always return a string that passes
// validRequestID, whether it echoes a valid inbound id or mints a fresh
// one. This is a stronger property than FuzzValidRequestID (which only
// checks the validator) and pins the header-injection defense: a minted
// id can never contain characters outside [a-zA-Z0-9_-] (no newlines,
// spaces, or control bytes that could forge a log line).
func FuzzRequestIDOrNew(f *testing.F) {
	f.Add("")
	f.Add("valid-id-123")
	f.Add("AAAA")
	f.Add("\x00\x01\x02")
	f.Add("a]b\nc")
	f.Add("way-too-long-" + string(make([]byte, 100)))
	f.Fuzz(func(t *testing.T, inbound string) {
		result := requestIDOrNew(inbound)
		if !validRequestID(result) {
			t.Errorf("requestIDOrNew(%q) = %q which fails validRequestID", inbound, result)
		}
	})
}
