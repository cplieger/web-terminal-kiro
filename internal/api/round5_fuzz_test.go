package api

import "testing"

// FuzzIsHiddenUnicodePartition checks that isHiddenUnicode and SanitizeUnicode
// agree: hidden runes are removed by SanitizeUnicode, non-hidden runes survive.
// Bug class: predicate/filter disagreement enabling prompt-injection bypass.
func FuzzIsHiddenUnicodePartition(f *testing.F) {
	f.Add('A')
	f.Add('\u200B')
	f.Add('\U000E0041')
	f.Add('\u00AD')
	f.Add('€')
	f.Add('\u2066')

	f.Fuzz(func(t *testing.T, r rune) {
		s := string(r)
		sanitized := SanitizeUnicode(s)
		if isHiddenUnicode(r) {
			if sanitized != "" {
				t.Errorf("isHiddenUnicode(%U)=true but SanitizeUnicode(%q)=%q (want empty)", r, s, sanitized)
			}
		} else {
			if sanitized != s {
				t.Errorf("isHiddenUnicode(%U)=false but SanitizeUnicode(%q)=%q (want %q)", r, s, sanitized, s)
			}
		}
	})
}

// FuzzSanitizeUnicodeIdempotent checks that SanitizeUnicode is a fixed point:
// applying it twice yields the same result as once.
// Bug class: incomplete removal of hidden chars in multi-byte sequences enabling staged prompt injection.
func FuzzSanitizeUnicodeIdempotent(f *testing.F) {
	f.Add("hello")
	f.Add("\u200B\u200C\u200D")
	f.Add("\U000E0041\U000E0042")
	f.Add("text\u2066dir\u2069end")
	f.Add("")
	f.Add("abc\u00ADdef")

	f.Fuzz(func(t *testing.T, s string) {
		once := SanitizeUnicode(s)
		twice := SanitizeUnicode(once)
		if once != twice {
			t.Errorf("SanitizeUnicode not idempotent: once=%q twice=%q", once, twice)
		}
	})
}
