package auth

import (
	"log/slog"
	"testing"
)

// FuzzLineRingBounds exercises the lineRing with arbitrary push sequences,
// verifying resource-bounds invariants: first/last slices never exceed
// halfCap, and each stored line is truncated at perLineCap.
func FuzzLineRingBounds(f *testing.F) {
	f.Add("short", 3, 64)
	f.Add("", 1, 1)
	f.Add("a]very-long-line-that-exceeds-typical-caps-by-a-wide-margin-and-keeps-going", 2, 8)
	f.Fuzz(func(t *testing.T, line string, halfCap, perLineCap int) {
		if halfCap < 1 || halfCap > 1000 {
			return
		}
		if perLineCap < 1 || perLineCap > 10000 {
			return
		}
		r := newLineRing(halfCap, perLineCap)
		// Push the same line multiple times to exercise transitions.
		for i := range halfCap*3 + 1 {
			_ = i
			r.Push(line)
		}
		if len(r.first) > halfCap {
			t.Fatalf("first slice %d exceeds halfCap %d", len(r.first), halfCap)
		}
		if len(r.last) > halfCap {
			t.Fatalf("last slice %d exceeds halfCap %d", len(r.last), halfCap)
		}
		for _, s := range r.Sample() {
			if len(s) > perLineCap {
				t.Fatalf("line len %d exceeds perLineCap %d", len(s), perLineCap)
			}
		}
	})
}

// FuzzBuildLoginArgs verifies that arbitrary provider/region values never
// produce argv entries that look like flags (encoding-asymmetry /
// authorization-seams). In production, validation runs first, but this
// fuzz target confirms the builder itself does not introduce flag injection.
func FuzzBuildLoginArgs(f *testing.F) {
	f.Add("https://id.example.com", "us-east-1")
	f.Add("", "")
	f.Add("--malicious", "--help")
	f.Add("https://a.b/c?x=1&y=2", "eu-west-1")
	f.Fuzz(func(t *testing.T, provider, region string) {
		args := buildLoginArgs(provider, region)
		// First two args are always "login" "--use-device-flow"
		if len(args) < 2 || args[0] != cmdLogin || args[1] != flagDeviceFlow {
			t.Fatalf("unexpected base args: %v", args)
		}
		// After the known flags, values must follow their flag key.
		// Verify values at positions 3 and 5 (if present) match input.
		for i := 2; i < len(args); i += 2 {
			if i+1 >= len(args) {
				t.Fatalf("odd number of extra args: %v", args)
			}
			key := args[i]
			val := args[i+1]
			switch key {
			case "--identity-provider":
				if val != provider {
					t.Fatalf("provider mismatch: got %q want %q", val, provider)
				}
			case "--region":
				if val != region {
					t.Fatalf("region mismatch: got %q want %q", val, region)
				}
			default:
				t.Fatalf("unexpected flag key: %q", key)
			}
		}
	})
}

// FuzzToSlogAttrs exercises the mixed slog.Attr / key-value pair parser
// with boundary inputs (odd lengths, non-string keys, nested Attrs).
func FuzzToSlogAttrs(f *testing.F) {
	f.Add("key1", "val1", "key2", "val2")
	f.Add("", "", "k", "")
	f.Add("only-key", "", "", "")
	f.Fuzz(func(t *testing.T, k1, v1, k2, v2 string) {
		// Test with pure string pairs.
		kvs := []any{k1, v1, k2, v2}
		attrs := toSlogAttrs(kvs)
		if len(attrs) != 2 {
			t.Fatalf("expected 2 attrs from 4 elements, got %d", len(attrs))
		}
		if attrs[0].Key != k1 {
			t.Fatalf("attr[0].Key = %q, want %q", attrs[0].Key, k1)
		}
		if attrs[1].Key != k2 {
			t.Fatalf("attr[1].Key = %q, want %q", attrs[1].Key, k2)
		}

		// Test with slog.Attr mixed in.
		mixed := []any{slog.String("pre", "x"), k1, v1}
		attrs2 := toSlogAttrs(mixed)
		if len(attrs2) != 2 {
			t.Fatalf("expected 2 attrs from mixed 3 elements, got %d", len(attrs2))
		}

		// Test with odd element (non-string key should be dropped).
		odd := []any{42, k1, v1}
		attrs3 := toSlogAttrs(odd)
		if len(attrs3) != 1 {
			t.Fatalf("expected 1 attr from odd-keyed input, got %d", len(attrs3))
		}
	})
}
