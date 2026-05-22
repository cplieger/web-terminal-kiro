package auth

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
)

// Auth-event audit logging. Emits structured slog records with a
// fixed `event_kind=auth` attribute so log-collection tooling (Loki,
// journald) can filter the audit trail from operational logs.
// Mirrors subflux's `internal/server/authhandlers.Audit` shape and
// intent at a smaller surface (vibecli's auth is just login + logout
// against kiro-cli; no passkeys, TOTP, OIDC, API keys, sessions).
//
// Why slog and not a DB table: vibecli is single-user, LAN-only. A
// SQLite audit_events table would be a feature with no consumer
// (no admin UI, no compliance requirement, no incident workflow);
// Loki retention covers the homelab forensic need at zero LOC of
// schema/migration/UI.
//
// Standard fields on every record:
//   - event_kind: "auth" (constant; the filter token)
//   - event:      one of the AuditEvent values (login.success, etc.)
//   - success:    bool
//   - ip:         client IP from r.RemoteAddr (port stripped)
//   - user_agent: request User-Agent header

// AuditEventKind is the fixed attribute value used to mark auth audit
// records. Filter on `event_kind="auth"` in log queries.
const AuditEventKind = "auth"

// AuditEvent enumerates the security-relevant events captured in the
// audit trail. vibecli's surface is small: login start/finish/timeout
// and logout. whoami is read-only (no auth state change) and is not
// audited.
type AuditEvent string

const (
	AuditLoginStart   AuditEvent = "login.start"
	AuditLoginSuccess AuditEvent = "login.success"
	AuditLoginFailure AuditEvent = "login.failure"
	AuditLoginBusy    AuditEvent = "login.busy"
	AuditLogout       AuditEvent = "logout"
)

// audit emits a structured auth audit record at the specified slog
// level. Failures should emit at WARN; successes at INFO.
//
// Extra attributes are appended verbatim. Use `slog.String("reason",
// "timeout")` etc. to keep the structured shape consistent.
func audit(r *http.Request, level slog.Level, event AuditEvent, success bool, kvs ...any) {
	attrs := make([]any, 0, 5+len(kvs))
	attrs = append(attrs,
		slog.String("event_kind", AuditEventKind),
		slog.String("event", string(event)),
		slog.Bool("success", success),
		slog.String("ip", clientIP(r)),
		slog.String("user_agent", r.UserAgent()),
	)
	attrs = append(attrs, kvs...)
	slog.LogAttrs(r.Context(), level, "audit", toSlogAttrs(attrs)...)
}

// clientIP strips the port from r.RemoteAddr; if no port is present,
// returns RemoteAddr as-is.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// toSlogAttrs converts a mixed slog.Attr / key-value slice into a
// pure []slog.Attr. slog.LogAttrs requires Attr (not the looser
// any-pair shape).
//
// Each entry must be either a slog.Attr or a (string, any) pair.
// Anything else is dropped with a debug log so a programming bug
// (caller passes the wrong types) is surfaced rather than emitted as
// an empty-key attribute.
func toSlogAttrs(kvs []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(kvs))
	for i := 0; i < len(kvs); i++ {
		v := kvs[i]
		if a, ok := v.(slog.Attr); ok {
			out = append(out, a)
			continue
		}
		key, ok := v.(string)
		if !ok {
			slog.Debug("audit: dropping non-attr/non-string key in audit kvs",
				"index", i, "type", fmt.Sprintf("%T", v))
			continue
		}
		var val any
		if i+1 < len(kvs) {
			val = kvs[i+1]
			i++
		}
		out = append(out, slog.Any(key, val))
	}
	return out
}
