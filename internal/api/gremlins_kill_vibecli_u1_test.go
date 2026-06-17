package api

import (
	"strings"
	"testing"
)

// Tests targeting the two CONDITIONALS_BOUNDARY mutants on
// middleware.go:114 inside validRequestID:
//
//	if len(s) < 1 || len(s) > 64 {
//	          ^col12        ^col26
//
// Both are length-boundary comparisons. The kill relies on exercising
// the exact equal-length inputs (len==1 and len==64) where `<` vs `<=`
// and `>` vs `>=` diverge.
//
//   - col 12 (`len(s) < 1` -> `len(s) <= 1`): at len 1 the original does
//     NOT short-circuit-reject (1 < 1 is false), so a valid 1-char id is
//     accepted (true). The mutant rejects it (1 <= 1 is true -> false).
//   - col 26 (`len(s) > 64` -> `len(s) >= 64`): at len 64 the original
//     does NOT reject (64 > 64 is false), so a valid 64-char id is
//     accepted (true). The mutant rejects it (64 >= 64 is true -> false).
//
// Expected booleans are hardcoded; inputs are built only by repetition,
// never by re-deriving the expected result.

// gk_vibecli_u1_valid64 is a 64-character all-valid request id (exactly
// at the upper inclusive boundary). 'a' is in the accepted [a-z] set, so
// rune validation passes and only the length comparison decides the
// outcome.
var gk_vibecli_u1_valid64 = strings.Repeat("a", 64)

func Test_gk_vibecli_u1_validRequestID_lengthBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// len 1: kills col-12 `<` -> `<=`. Original accepts (true);
		// mutant rejects (false).
		{name: "single_valid_char_at_lower_boundary", in: "a", want: true},
		// len 64: kills col-26 `>` -> `>=`. Original accepts (true);
		// mutant rejects (false).
		{name: "sixtyfour_valid_chars_at_upper_boundary", in: gk_vibecli_u1_valid64, want: true},
		// Context only (both original and mutant agree): len 0 is
		// rejected, len 65 is rejected. These pin the surrounding
		// behavior but do not by themselves distinguish the mutants.
		{name: "empty_rejected", in: "", want: false},
		{name: "sixtyfive_chars_rejected", in: strings.Repeat("a", 65), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validRequestID(tc.in)
			if got != tc.want {
				t.Errorf("validRequestID(len=%d) = %v, want %v", len(tc.in), got, tc.want)
			}
		})
	}
}
