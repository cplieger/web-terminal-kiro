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
