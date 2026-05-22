package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodegenDrift verifies that the committed .gen.ts files match what
// wire-codegen would produce. If this test fails, run:
//
//	go run ./cmd/wire-codegen
func TestCodegenDrift(t *testing.T) {
	outDir := filepath.Join("..", "..", "static-src", "wire")

	cases := []struct {
		file     string
		generate func(*strings.Builder)
	}{
		{"types.gen.ts", generateTypes},
		{"decoders.gen.ts", generateDecoders},
		{"constants.gen.ts", generateConstants},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			var buf strings.Builder
			tc.generate(&buf)
			want := buf.String()

			committed, err := os.ReadFile(filepath.Join(outDir, tc.file))
			if err != nil {
				t.Fatalf("read committed %s: %v", tc.file, err)
			}
			if got := string(committed); got != want {
				t.Errorf("%s is out of date; run: go run ./cmd/wire-codegen\n"+
					"first diff at byte %d", tc.file, firstDiff(got, want))
			}
		})
	}
}

func firstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}
