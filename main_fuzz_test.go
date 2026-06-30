package main

import (
	"os"
	"testing"
)

// FuzzEnvOrPartition checks envOr returns os.Getenv when non-empty, fallback otherwise.
// Bug class: configuration injection where empty env var handling differs across platforms.
func FuzzEnvOrPartition(f *testing.F) {
	f.Add("HOME", "default")
	f.Add("NONEXISTENT_VAR_XYZ_FUZZ", "/workspace")
	f.Add("", "fallback")
	f.Add("PATH", "")
	f.Add("LANG", "C")
	f.Add("FUZZ_UNSET_12345", "value")

	f.Fuzz(func(t *testing.T, key, fallback string) {
		got := envOr(key, fallback)
		env := os.Getenv(key)

		if env != "" {
			if got != env {
				t.Errorf("envOr(%q, %q) = %q, want os.Getenv=%q", key, fallback, got, env)
			}
		} else {
			if got != fallback {
				t.Errorf("envOr(%q, %q) = %q, want fallback=%q", key, fallback, got, fallback)
			}
		}
	})
}

// TestEnvOr_emptyEnvFallsBackToDefault pins envOr with hardcoded expectations:
// a set, non-empty variable wins; an unset variable falls back; and a variable
// set to the EMPTY STRING is treated as unset and also falls back. That
// empty-string-as-unset case is the deliberate behavior FuzzEnvOrPartition
// never reaches (it never sets a variable to "", and the fuzzer cannot drive
// the process environment) and which its os.Getenv-mirroring oracle does not
// pin as a contract.
func TestEnvOr_emptyEnvFallsBackToDefault(t *testing.T) {
	const key = "VIBECLI_ENVOR_TEST_KEY"
	cases := []struct {
		name  string
		value string
		set   bool
		want  string
	}{
		{name: "set and non-empty returns the value", value: "chosen", set: true, want: "chosen"},
		{name: "unset returns the fallback", set: false, want: "fallback"},
		{name: "set to empty string is treated as unset", value: "", set: true, want: "fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.value)
			}
			if got := envOr(key, "fallback"); got != tc.want {
				t.Errorf("envOr(%q, %q) = %q, want %q", key, "fallback", got, tc.want)
			}
		})
	}
}
