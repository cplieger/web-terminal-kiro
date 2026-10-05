package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/toolbelt/v3"
)

// githubToken returns the token the tools engine sends with its api.github.com
// requests and the variable it came from: GH_TOKEN, else GITHUB_TOKEN, gh's own
// order for github.com. Only an empty value counts as unset. The bytes are sent
// as set, because GitHub compares them: a value net/http cannot put in a header
// fails each request rather than authenticating as something not configured.
func githubToken() (token string, key envx.Key) {
	for _, k := range [...]envx.Key{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := envx.String(k); v != "" {
			return v, k
		}
	}
	return "", ""
}

// githubTokenSource adapts a fixed token to toolbelt.Config.GitHubToken. Nil
// for "" is toolbelt's anonymous mode.
func githubTokenSource(token string) func(context.Context) (string, error) {
	if token == "" {
		return nil
	}
	return func(context.Context) (string, error) { return token, nil }
}

// announceGitHubToken records which variable supplied the GitHub token, never
// its value.
func announceGitHubToken(key envx.Key) {
	if key != "" {
		slog.Info("tools: GitHub API requests carry a token", "env", string(key))
		return
	}
	slog.Info("tools: GitHub API requests carry no token; GitHub allows 60 an hour per IP address",
		"hint", "set GH_TOKEN to a GitHub token in the container environment to raise the limit")
}

// githubLimitReporter turns GitHub rate-limit job failures into one Warn per
// refusal GitHub sent. observe runs under toolbelt's queue lock, so it only
// hands the refusal to run's goroutine.
type githubLimitReporter struct {
	// until is when the engine next sends a request with the credential last
	// refused; an equal refusal before then is the engine's copy of it.
	until    time.Time
	refusals chan toolbelt.GitHubRateLimit
	// tokenEnv names the variable the token came from, "" when none was set.
	tokenEnv envx.Key
	last     toolbelt.GitHubRateLimit
}

// minRefusalHold is toolbelt's floor on how long it holds a refused
// credential's requests (githubapi.go minRateLimitWait).
const minRefusalHold = time.Minute

func newGitHubLimitReporter(tokenEnv envx.Key) *githubLimitReporter {
	return &githubLimitReporter{refusals: make(chan toolbelt.GitHubRateLimit, 1), tokenEnv: tokenEnv}
}

// observe is the Config.OnJobChanged half. A send that would block is dropped:
// the pending refusal already carries the same news.
func (r *githubLimitReporter) observe(j *toolbelt.Job) {
	if j.State != toolbelt.JobFailed || j.ErrorCode != toolbelt.ErrorCodeGitHubRateLimited || j.RateLimit == nil {
		return
	}
	select {
	case r.refusals <- *j.RateLimit:
	default:
	}
}

// run logs queued refusals until ctx ends.
func (r *githubLimitReporter) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rl := <-r.refusals:
			r.report(rl, time.Now())
		}
	}
}

// report logs rl unless it is the engine's copy of the last refusal: until the
// reset, and for at least minRefusalHold, the engine answers each request with
// that credential with one instead of sending it. Called from run only.
func (r *githubLimitReporter) report(rl toolbelt.GitHubRateLimit, now time.Time) {
	if rl == r.last && now.Before(r.until) {
		return
	}
	r.last, r.until = rl, now.Add(minRefusalHold)
	attrs := []any{"secondary", rl.Secondary}
	wait := "for at least a minute, because GitHub named no reset time"
	if rl.ResetAt > 0 {
		reset := time.UnixMilli(rl.ResetAt)
		if reset.After(r.until) {
			r.until = reset
		}
		attrs = append(attrs, "reset_at", reset.UTC().Format(time.RFC3339))
		wait = "until reset_at"
	}
	if rl.Limit > 0 {
		attrs = append(attrs, "limit", rl.Limit)
	}
	if rl.Authenticated {
		attrs = append(attrs, "token_env", string(r.tokenEnv),
			"hint", "the tools engine sends no GitHub API request with this token "+wait)
		slog.Warn("tools: GitHub's rate limit for the configured token was reached; tool version checks and GitHub release installs fail until it resets", attrs...)
		return
	}
	attrs = append(attrs,
		"hint", "the tools engine sends no GitHub API request without a token "+wait+". To raise the limit, set GH_TOKEN (or GITHUB_TOKEN) to a GitHub token in the container environment and recreate the container: GitHub allows 60 requests an hour per IP address without a token and 5,000 with a personal access token")
	slog.Warn("tools: GitHub's rate limit for requests without a token was reached; tool version checks and GitHub release installs fail until it resets", attrs...)
}
