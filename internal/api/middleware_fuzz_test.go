package api

import "testing"

func FuzzValidRequestID(f *testing.F) {
	f.Add("")
	f.Add("abc-123_XYZ")
	f.Add("a")
	f.Add("toolong" + string(make([]byte, 65)))
	f.Add("has spaces")
	f.Add("has\nnewline")
	f.Add("../traversal")
	f.Add("--help")
	f.Fuzz(func(t *testing.T, s string) {
		ok := validRequestID(s)
		if ok {
			if len(s) < 1 || len(s) > 64 {
				t.Errorf("validRequestID accepted out-of-range length %d", len(s))
			}
			for _, r := range s {
				switch {
				case r >= 'a' && r <= 'z':
				case r >= 'A' && r <= 'Z':
				case r >= '0' && r <= '9':
				case r == '-' || r == '_':
				default:
					t.Errorf("validRequestID accepted invalid rune %q in %q", r, s)
				}
			}
		}
	})
}
