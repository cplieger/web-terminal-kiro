package auth

import (
	"bytes"
	"testing"
)

// FuzzLimitedWriter exercises the limitedWriter byte-cap enforcement.
// Bug class: buffer overflow / resource exhaustion — if limitedWriter
// allows more than N bytes through, a hostile subprocess can OOM the
// container.
func FuzzLimitedWriter(f *testing.F) {
	f.Add([]byte("hello"), int64(3))
	f.Add([]byte(""), int64(0))
	f.Add([]byte("abcdef"), int64(6))
	f.Add([]byte("x"), int64(100))
	f.Add([]byte("overflow attempt with long payload"), int64(5))
	f.Add([]byte("\x00\xff\xfe"), int64(1))
	f.Fuzz(func(t *testing.T, data []byte, cap int64) {
		if cap < 0 || cap > 1<<20 {
			return
		}
		var buf bytes.Buffer
		lw := &limitedWriter{W: &buf, N: cap}

		// Write in chunks of varying size to stress boundary logic.
		remaining := data
		for len(remaining) > 0 {
			chunk := remaining
			if len(chunk) > 7 {
				chunk = remaining[:7]
			}
			_, _ = lw.Write(chunk)
			remaining = remaining[len(chunk):]
		}

		// Invariant: underlying buffer never exceeds original cap.
		if int64(buf.Len()) > cap {
			t.Errorf("limitedWriter allowed %d bytes through cap %d", buf.Len(), cap)
		}
	})
}

// FuzzHumanizeAccountType verifies the "Logged in with " prefix
// invariant. All returned strings must start with that prefix regardless
// of input.
// Bug class: inconsistent UI labelling / missing generic fallback causing
// empty or malformed auth display strings.
func FuzzHumanizeAccountType(f *testing.F) {
	f.Add("builderid")
	f.Add("identitycenter")
	f.Add("iamidentitycenter")
	f.Add("social")
	f.Add("BUILDERID")
	f.Add("unknown_type")
	f.Add("")
	f.Fuzz(func(t *testing.T, accountType string) {
		result := humanizeAccountType(accountType)
		const prefix = "Logged in with "
		if len(result) < len(prefix) || result[:len(prefix)] != prefix {
			t.Errorf("humanizeAccountType(%q) = %q, missing prefix %q", accountType, result, prefix)
		}
	})
}

// FuzzParseWhoamiFields verifies field extraction consistency: if both
// snake_case and camelCase keys are present, snake_case takes priority
// for AccountType, and lowercase "email" takes priority for Email.
// Bug class: field aliasing confusion / data source priority inversion
// leading to wrong identity displayed.
func FuzzParseWhoamiFields(f *testing.F) {
	f.Add("builderid", "camelVal", "user@example.com", "Other@x.com")
	f.Add("", "", "", "")
	f.Add("identitycenter", "", "", "alt@b.com")
	f.Add("", "social", "a@b.c", "")
	f.Add("X", "Y", "Z", "W")
	f.Fuzz(func(t *testing.T, snakeAcct, camelAcct, lowerEmail, capEmail string) {
		info := map[string]any{}
		if snakeAcct != "" {
			info["account_type"] = snakeAcct
		}
		if camelAcct != "" {
			info["accountType"] = camelAcct
		}
		if lowerEmail != "" {
			info["email"] = lowerEmail
		}
		if capEmail != "" {
			info["Email"] = capEmail
		}

		fields := parseWhoamiFields(info)

		// Invariant: snake_case account_type takes priority when non-empty.
		if snakeAcct != "" && fields.AccountType != snakeAcct {
			t.Errorf("snake_case priority violated: got %q, want %q", fields.AccountType, snakeAcct)
		}
		// Invariant: lowercase email takes priority when non-empty.
		if lowerEmail != "" && fields.Email != lowerEmail {
			t.Errorf("lowercase email priority violated: got %q, want %q", fields.Email, lowerEmail)
		}
		// Invariant: when snake_case is empty, camelCase is used.
		if snakeAcct == "" && camelAcct != "" && fields.AccountType != camelAcct {
			t.Errorf("camelCase fallback failed: got %q, want %q", fields.AccountType, camelAcct)
		}
	})
}
