// handle_whoami.go implements the GET /api/whoami endpoint.

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"

	"vibecli/internal/api"
)

// WhoamiErrorResponse is the typed error envelope for handleWhoami error
// paths. Replaces anonymous map[string]string literals so the wire shape
// is compiler-checked and consistent with login/logout typed responses.
type WhoamiErrorResponse struct {
	Error string `json:"error"`
}

// handleWhoami shells out to `kiro-cli whoami --format json` and returns
// the parsed JSON with an added "auth" field humanised via
// humanizeAccountType. Fails soft on command or parse error — the
// client uses this for a banner, not an auth gate; we write HTTP 200
// with an "error" field so the UI can render a "not logged in" state.
func (h *Handler) handleWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		api.MethodNotAllowed(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeouts.Whoami)
	defer cancel()
	// h.cliPath is operator-controlled (resolved at server start
	// from the bundled binary path), never user input.
	cmd := exec.CommandContext(ctx, h.cliPath, "whoami", "--format", "json") // #nosec G204 -- cliPath is operator-controlled
	var stderr bytes.Buffer
	var stdoutBuf bytes.Buffer
	cmd.Stderr = &limitedWriter{W: &stderr, N: stderrCap}
	cmd.Stdout = &limitedWriter{W: &stdoutBuf, N: whoamiMaxOutput}
	cliErr := runCLI(ctx, cmd)
	out := stdoutBuf.Bytes()
	if cliErr != nil {
		switch cliErr.Kind {
		case CLIErrTimeout:
			attrs := make([]any, 0, 4)
			attrs = append(attrs, "timeout", h.timeouts.Whoami)
			attrs = append(attrs, stderrAttr(&stderr)...)
			slog.Warn("whoami: kiro-cli timed out", attrs...)
		case CLIErrNotFound:
			slog.Warn("whoami: kiro-cli binary not found",
				"cli_path", h.cliPath)
		default:
			attrs := make([]any, 0, 6)
			attrs = append(attrs, "error", cliErr.Err, "stdout_bytes", len(out))
			attrs = append(attrs, stderrAttr(&stderr)...)
			slog.Warn("whoami: kiro-cli invocation failed", attrs...)
		}
		api.WriteJSON(w, WhoamiErrorResponse{Error: "whoami unavailable"})
		return
	}
	info, err := whoamiInfo(out)
	if err != nil {
		slog.Warn("whoami: cli output not parseable as json",
			"error", err, "stdout_bytes", len(out))
		api.WriteJSON(w, WhoamiErrorResponse{Error: "whoami unavailable"})
		return
	}
	api.WriteJSON(w, info)
}

// whoamiFields is a typed intermediate struct for extracting known fields
// from the kiro-cli whoami JSON. It encapsulates the snake_case/camelCase
// aliasing logic so field access is compiler-checked.
type whoamiFields struct {
	AccountType string
	Email       string
}

// parseWhoamiFields extracts known fields from the decoded map, handling
// both snake_case (earlier CLI) and camelCase (kiro-cli 2.0.1+) variants.
func parseWhoamiFields(info map[string]any) whoamiFields {
	var f whoamiFields
	if at, ok := info["account_type"].(string); ok && at != "" {
		f.AccountType = at
	} else if camel, ok := info["accountType"].(string); ok {
		f.AccountType = camel
	}
	if email, ok := info["email"].(string); ok && email != "" {
		f.Email = email
	} else if capEmail, ok := info["Email"].(string); ok {
		f.Email = capEmail
	}
	return f
}

// whoamiInfo parses kiro-cli's --format json whoami output and
// normalises account_type to the stable UI "auth" label. Accepts both
// snake_case (earlier CLI) and camelCase (kiro-cli 2.0.1+) field names.
// A null JSON payload becomes an empty map. kiro-cli 2.0.1+ appends a
// non-JSON footer after the JSON payload (e.g. a "Profile: ..." banner
// from Identity Center); decoding via json.Decoder consumes exactly
// one JSON value so the trailing bytes are ignored.
func whoamiInfo(out []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var info map[string]any
	if err := dec.Decode(&info); err != nil {
		return nil, err
	}
	if info == nil {
		info = map[string]any{}
	}
	fields := parseWhoamiFields(info)
	if fields.AccountType != "" {
		info["auth"] = humanizeAccountType(fields.AccountType)
	}
	if _, exists := info["email"]; !exists && fields.Email != "" {
		info["email"] = fields.Email
	}
	return info, nil
}

// humanizeAccountType turns kiro-cli's enum values into the same
// phrasing the plaintext output uses.
const (
	authBuilderID      = "Logged in with Builder ID"
	authIdentityCenter = "Logged in with IAM Identity Center"
	authSocialLogin    = "Logged in with social login"
	authPrefixGeneric  = "Logged in with "
)

// accountTypeLabels maps kiro-cli account_type enum values (lowercased)
// to their human-readable labels. Data-driven: adding a new account type
// is a single map entry, not a new case statement.
const (
	acctBuilderID         = "builderid"
	acctIdentityCenter    = "identitycenter"
	acctIAMIdentityCenter = "iamidentitycenter"
	acctSocial            = "social"
)

var accountTypeLabels = map[string]string{
	acctBuilderID:         authBuilderID,
	acctIdentityCenter:    authIdentityCenter,
	acctIAMIdentityCenter: authIdentityCenter,
	acctSocial:            authSocialLogin,
}

func humanizeAccountType(t string) string {
	if label, ok := accountTypeLabels[strings.ToLower(t)]; ok {
		return label
	}
	return authPrefixGeneric + t
}
