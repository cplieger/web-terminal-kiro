package api

import (
	"testing"
)

// FuzzStripANSIIdempotent verifies that StripANSI is idempotent:
// applying it twice yields the same result as once. This is distinct
// from FuzzSanitizeOutputIdempotent which also involves Unicode
// sanitisation.
// Bug class: incomplete ANSI stripping where a partial escape sequence
// survives the first pass and becomes a valid sequence after truncation,
// leading to escape sequence reconstruction on re-application.
func FuzzStripANSIIdempotent(f *testing.F) {
	f.Add("")
	f.Add("plain text")
	f.Add("\x1b[31mred\x1b[0m")
	f.Add("\x1b]0;title\x07rest")
	f.Add("\x1b[38;2;255;0;0m\x1b[1m\x1b[0m")
	f.Add("\x1b[\x1b[1m")
	f.Fuzz(func(t *testing.T, s string) {
		once := StripANSI(s)
		twice := StripANSI(once)
		if once != twice {
			t.Errorf("StripANSI not idempotent:\n  input: %q\n  once:  %q\n  twice: %q", s, once, twice)
		}
	})
}

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
