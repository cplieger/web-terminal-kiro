package api

import (
	"testing"
)

// FuzzRequestIDOrNew verifies that requestIDOrNew always returns a
// string that passes validRequestID. This is a stronger property than
// FuzzValidRequestID which only checks the validator — here we verify
// the generator's output is always in the valid set.
// Bug class: header injection via generated request ID that contains
// characters outside the allowed set (newlines, spaces, etc.) when
// the inbound header is invalid and a new ID must be minted.
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
