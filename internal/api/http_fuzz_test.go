package api

import "testing"

func FuzzStripANSI(f *testing.F) {
	f.Add("")
	f.Add("hello world")
	f.Add("\x1b[31mred\x1b[0m")
	f.Add("\x1b]0;title\x07rest")
	f.Add("\x1b(B\x1b[m")
	f.Add("\x1b[?25h\x1b[?25l")
	f.Fuzz(func(t *testing.T, s string) {
		_ = StripANSI(s)
	})
}

func FuzzSanitizeOutput(f *testing.F) {
	f.Add("")
	f.Add("clean text")
	f.Add("\x1b[1mbold\x1b[0m")
	f.Add("zero\u200Bwidth")
	f.Add("\x1b[31m\u200Bmixed\x1b[0m")
	f.Fuzz(func(t *testing.T, s string) {
		_ = SanitizeOutput(s)
	})
}

func FuzzSanitizeUnicode(f *testing.F) {
	f.Add("hello world")
	f.Add("")
	f.Add("emoji 🎉 ok")
	f.Add("tag\U000E0041char")      // TAG LATIN CAPITAL LETTER A
	f.Add("zero\u200Bwidth")        // zero-width space
	f.Add("bidi\u202Atext\u202C")   // LTR embedding + pop
	f.Add("isolate\u2066dir\u2069") // LTR isolate + pop
	f.Add("soft\u00ADhyphen")       // soft hyphen
	f.Add("joiner\u2060word")       // word joiner
	f.Fuzz(func(t *testing.T, s string) {
		out := SanitizeUnicode(s)
		for _, r := range out {
			if isHiddenUnicode(r) {
				t.Errorf("SanitizeUnicode(%q) contains hidden rune %U", s, r)
			}
		}
	})
}

func BenchmarkStripANSI(b *testing.B) {
	cases := []struct {
		name, input string
	}{
		{"clean", "hello world no ansi here at all"},
		{"light", "prefix \x1b[31mred\x1b[0m suffix"},
		{"heavy", "\x1b[?25l\x1b[1;1H\x1b[38;2;255;0;0m█\x1b[39m\x1b[2;1H\x1b[32mok\x1b[0m\x1b[3;1H\x1b]0;title\x07\x1b[?25h"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_ = StripANSI(tc.input)
			}
		})
	}
}

func FuzzSanitizeOutputIdempotent(f *testing.F) {
	f.Add("")
	f.Add("clean text")
	f.Add("\x1b[1mbold\x1b[0m")
	f.Add("zero\u200Bwidth")
	f.Add("\x1b[31m\u200Bmixed\x1b[0m")
	f.Add("nested\x1b[38;2;1;2;3m\U000E0041end")
	f.Fuzz(func(t *testing.T, s string) {
		once := SanitizeOutput(s)
		twice := SanitizeOutput(once)
		if once != twice {
			t.Errorf("SanitizeOutput not idempotent:\n  input: %q\n  once:  %q\n  twice: %q", s, once, twice)
		}
	})
}
