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
