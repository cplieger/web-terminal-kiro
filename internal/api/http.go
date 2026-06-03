// Package api holds the HTTP response helpers and output sanitisers
// shared by the server and the auth handler. Ported from
// apps/vibekit/web/internal/api/http.go (subset — only what vibecli's
// auth + terminal subsystems actually use). See vibekit for the full
// version with atomic-write helpers and ServeJSONFile.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// MaxJSONBody is the maximum size for JSON request bodies (1 MiB).
const MaxJSONBody = 1024 * 1024

// LimitBody wraps r.Body with MaxBytesReader to prevent oversized requests.
func LimitBody(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
}

// --- Terminal sanitisers ---

// ansiRe matches ANSI escape sequences produced by kiro-cli and its
// subprocesses. Covers CSI, OSC, charset-select, and single-character
// control forms. 8-bit C1 controls are not matched (invalid UTF-8 for
// Go's regexp engine); kiro-cli always uses the 7-bit ESC-prefixed
// forms. Verbatim from vibekit/internal/api/http.go.
var ansiRe = regexp.MustCompile(
	`\x1b\[[0-9;?]*[a-zA-Z]` + // CSI
		`|\x1b\][\s\S]*?(?:\x07|\x1b\\)` + // OSC
		`|\x1b[()][A-Za-z0-9]` + // charset select
		`|\x1b[NOPX^_78=>c]`, // SS2/SS3/DCS/SOS/PM/APC/save/restore/RIS/keypad
)

// StripANSI removes ANSI escape sequences from a string.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// SanitizeUnicode strips hidden Unicode characters used for prompt
// injection via tool output: TAG characters (U+E0000-E007F),
// zero-width spaces/joiners, bidi controls, format controls, and soft
// hyphens. Matches Q Developer CLI's ExecuteCmd sanitisation.
func SanitizeUnicode(s string) string {
	return strings.Map(func(r rune) rune {
		if isHiddenUnicode(r) {
			return -1
		}
		return r
	}, s)
}

// isHiddenUnicode reports whether r is an invisible Unicode codepoint
// used for prompt injection.
func isHiddenUnicode(r rune) bool {
	if r >= 0xE0000 && r <= 0xE007F {
		return true // TAG characters
	}
	switch r {
	case 0x00AD, // soft hyphen
		0x200B, 0x200C, 0x200D, // zero-width space/non-joiner/joiner
		0x200E, 0x200F, // LTR/RTL marks
		0xFEFF,                                 // BOM / zero-width no-break space
		0x2060, 0x2061, 0x2062, 0x2063, 0x2064: // word joiner + invisible math
		return true
	}
	if r >= 0x202A && r <= 0x202E {
		return true // bidi embedding/override
	}
	if r >= 0x2066 && r <= 0x2069 {
		return true // bidi isolate
	}
	return false
}

// SanitizeOutput applies both ANSI stripping and Unicode sanitization,
// iterating to a fixed point. A single pass is not enough: removing a
// hidden Unicode char (e.g. a zero-width space inside "\x1b(\u200b0")
// can complete an escape sequence that the next StripANSI pass then
// strips. Iterating guarantees the result is fully sanitized — no
// residual escapes an attacker hid behind zero-width chars — and makes
// the function idempotent. Each pass only removes runes, so length
// strictly decreases until stable; it terminates in O(len(s)) passes.
// Use on all subprocess output before echoing to clients or logs.
func SanitizeOutput(s string) string {
	for {
		out := SanitizeUnicode(StripANSI(s))
		if out == s {
			return out
		}
		s = out
	}
}

// --- JSON response writers ---

func jsonHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// WriteJSON encodes v as JSON with status 200.
func WriteJSON(w http.ResponseWriter, v any) {
	WriteJSONStatus(w, http.StatusOK, v)
}

// WriteJSONStatus encodes v with the given status code. Encode
// failures after the status has been committed are logged at Warn.
func WriteJSONStatus(w http.ResponseWriter, code int, v any) {
	jsonHeaders(w)
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("api: json encode failed after status committed",
			"code", code, "error", err)
	}
}

// --- Named error responses ---
//
// These keep the historical `error` field on the JSON wire format and
// add `code` + `request_id` (when a request context is available) for
// machine-readable handling. Existing clients that only read .error
// continue to work unchanged.

// BadRequest writes a 400. Prefer this signature where r is in scope.
func BadRequest(w http.ResponseWriter, r *http.Request, msg string) {
	WriteError(w, r, http.StatusBadRequest, "bad_request", msg)
}

// Conflict writes a 409.
func Conflict(w http.ResponseWriter, r *http.Request, msg string) {
	WriteError(w, r, http.StatusConflict, "conflict", msg)
}

// MethodNotAllowed writes a 405.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

// Ok writes a 200 with {"ok": true}.
func Ok(w http.ResponseWriter) {
	WriteJSON(w, map[string]bool{"ok": true})
}
