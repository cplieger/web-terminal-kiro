package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/slogx/capture"
	"github.com/cplieger/toolbelt/v3"
)

// Serial: t.Setenv forbids t.Parallel.
func TestGitHubToken_readsGHTokenThenGITHUBToken(t *testing.T) {
	for name, tc := range map[string]struct {
		ghToken     string
		githubToken string
		wantToken   string
		wantKey     envx.Key
	}{
		"neither set means no token": {},
		"GH_TOKEN alone":             {ghToken: "gh-tok", wantToken: "gh-tok", wantKey: "GH_TOKEN"},
		"GITHUB_TOKEN alone":         {githubToken: "github-tok", wantToken: "github-tok", wantKey: "GITHUB_TOKEN"},
		"GH_TOKEN wins over GITHUB_TOKEN": {
			ghToken: "gh-tok", githubToken: "github-tok", wantToken: "gh-tok", wantKey: "GH_TOKEN",
		},
		"a whitespace-only GH_TOKEN still wins over GITHUB_TOKEN": {
			ghToken: " \n", githubToken: "github-tok", wantToken: " \n", wantKey: "GH_TOKEN",
		},
		"a trailing newline is kept": {ghToken: "gh-tok\n", wantToken: "gh-tok\n", wantKey: "GH_TOKEN"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", tc.ghToken)
			t.Setenv("GITHUB_TOKEN", tc.githubToken)
			token, key := githubToken()
			if token != tc.wantToken || key != tc.wantKey {
				t.Errorf("githubToken() = (%q, %q), want (%q, %q)", token, key, tc.wantToken, tc.wantKey)
			}
		})
	}
}

func TestToolsConfig_sendsTheConfiguredGitHubToken(t *testing.T) {
	t.Parallel()
	cfg := toolsConfig(&baseTools{githubToken: "gh-tok", githubTokenEnv: "GH_TOKEN"}, t.TempDir(), nil)
	if cfg.GitHubToken == nil {
		t.Fatal("toolsConfig(token gh-tok).GitHubToken = nil, want a source returning the token")
	}
	got, err := cfg.GitHubToken(t.Context())
	if got != "gh-tok" || err != nil {
		t.Errorf("toolsConfig(token gh-tok).GitHubToken() = (%q, %v), want (%q, nil)", got, err, "gh-tok")
	}
}

func TestToolsConfig_withoutATokenGoesAnonymous(t *testing.T) {
	t.Parallel()
	cfg := toolsConfig(&baseTools{}, t.TempDir(), nil)
	if cfg.GitHubToken == nil {
		return
	}
	got, err := cfg.GitHubToken(t.Context())
	if got != "" || err != nil {
		t.Errorf("toolsConfig(no token).GitHubToken() = (%q, %v), want (\"\", nil): an error would fail every GitHub request instead of sending it anonymously", got, err)
	}
}

// A configured token must reach no log line, while the boot record still says
// which variable supplied it. Serial: capture.Default mutates the global logger.
func TestStartTools_namesTheTokenVariableNeverTheToken(t *testing.T) {
	const secret = "gh-tok-never-logged"
	records := capture.Default(t)
	dir := t.TempDir()
	rt := startTools(t.Context(), &baseTools{
		configDir:      dir,
		catalogPath:    filepath.Join(dir, "absent-catalog.json"),
		githubToken:    secret,
		githubTokenEnv: "GH_TOKEN",
	})
	if rt.engine == nil {
		t.Fatal("engine is nil for an existing config dir; want a running tools engine")
	}
	t.Cleanup(rt.close)

	if got, ok := records.AttrValue("tools: GitHub API requests carry a token", "env"); !ok || got != "GH_TOKEN" {
		t.Errorf("token record env = %q (present=%v), want %q; log = %q", got, ok, "GH_TOKEN", records.Messages())
	}
	for _, r := range records.Records() {
		leaked := strings.Contains(r.Message, secret)
		r.Attrs(func(a slog.Attr) bool {
			leaked = leaked || strings.Contains(a.Value.String(), secret)
			return !leaked
		})
		if leaked {
			t.Errorf("log record %q carries the token value", r.Message)
		}
	}
}

func TestStartTools_saysWhenGitHubRequestsCarryNoToken(t *testing.T) {
	records := capture.Default(t)
	dir := t.TempDir()
	rt := startTools(t.Context(), &baseTools{configDir: dir, catalogPath: filepath.Join(dir, "absent-catalog.json")})
	if rt.engine == nil {
		t.Fatal("engine is nil for an existing config dir; want a running tools engine")
	}
	t.Cleanup(rt.close)

	const msg = "tools: GitHub API requests carry no token"
	if got := records.CountLevel(slog.LevelInfo, msg); got != 1 {
		t.Fatalf("log = %q, want exactly one %q Info (got %d)", records.Messages(), msg, got)
	}
	if !records.AttrContains(msg, "hint", "GH_TOKEN") {
		t.Errorf("no-token record hint does not name GH_TOKEN; log = %q", records.Messages())
	}
}

// rateLimitedJob is a failed job in the shape toolbelt reports for a GitHub
// rate-limit refusal.
func rateLimitedJob(rl toolbelt.GitHubRateLimit) *toolbelt.Job {
	return &toolbelt.Job{
		State:     toolbelt.JobFailed,
		Error:     "GitHub API rate limit reached",
		ErrorCode: toolbelt.ErrorCodeGitHubRateLimited,
		RateLimit: &rl,
	}
}

func TestGitHubLimitReporter_observeQueuesOnlyRateLimitFailures(t *testing.T) {
	t.Parallel()
	r := newGitHubLimitReporter("")
	r.observe(&toolbelt.Job{State: toolbelt.JobDone})
	r.observe(&toolbelt.Job{State: toolbelt.JobFailed, Error: "checksum mismatch"})
	select {
	case rl := <-r.refusals:
		t.Fatalf("observe queued %+v for a job no rate limit failed", rl)
	default:
	}

	want := toolbelt.GitHubRateLimit{ResetAt: 1791192600000, Limit: 60}
	r.observe(rateLimitedJob(want))
	select {
	case got := <-r.refusals:
		if got != want {
			t.Errorf("observe queued %+v, want %+v", got, want)
		}
	default:
		t.Fatal("observe queued nothing for a rate-limited job")
	}
}

// observe runs under toolbelt's queue lock, so a pending refusal must make it
// drop the next one rather than wait.
func TestGitHubLimitReporter_observeNeverBlocks(t *testing.T) {
	t.Parallel()
	r := newGitHubLimitReporter("")
	done := make(chan struct{})
	go func() {
		for range 3 {
			r.observe(rateLimitedJob(toolbelt.GitHubRateLimit{ResetAt: 1}))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observe blocked with a refusal already pending; it runs under the engine's queue lock")
	}
}

// Serial: capture.Default mutates the global logger.
func TestGitHubLimitReporter_reportNamesTheFixForAnonymousRequests(t *testing.T) {
	records := capture.Default(t)
	r := newGitHubLimitReporter("")
	resetAt := time.Date(2026, 10, 5, 21, 30, 0, 0, time.UTC)
	r.report(toolbelt.GitHubRateLimit{ResetAt: resetAt.UnixMilli(), Limit: 60}, resetAt.Add(-time.Hour))

	const msg = "tools: GitHub's rate limit for requests without a token was reached"
	if got := records.CountLevel(slog.LevelWarn, msg); got != 1 {
		t.Fatalf("log = %q, want exactly one %q Warn (got %d)", records.Messages(), msg, got)
	}
	if got, _ := records.AttrValue(msg, "reset_at"); got != "2026-10-05T21:30:00Z" {
		t.Errorf("reset_at = %q, want %q", got, "2026-10-05T21:30:00Z")
	}
	if got, _ := records.AttrValue(msg, "limit"); got != "60" {
		t.Errorf("limit = %q, want %q", got, "60")
	}
	if !records.AttrContains(msg, "hint", "GH_TOKEN") {
		t.Errorf("hint does not name GH_TOKEN as the fix; log = %q", records.Messages())
	}
}

// Serial: capture.Default mutates the global logger.
func TestGitHubLimitReporter_reportNamesTheTokenVariableWhenOneWasSent(t *testing.T) {
	records := capture.Default(t)
	r := newGitHubLimitReporter("GITHUB_TOKEN")
	resetAt := time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)
	r.report(toolbelt.GitHubRateLimit{ResetAt: resetAt.UnixMilli(), Authenticated: true}, resetAt.Add(-time.Hour))

	const msg = "tools: GitHub's rate limit for the configured token was reached"
	if got := records.CountLevel(slog.LevelWarn, msg); got != 1 {
		t.Fatalf("log = %q, want exactly one %q Warn (got %d)", records.Messages(), msg, got)
	}
	if got, _ := records.AttrValue(msg, "token_env"); got != "GITHUB_TOKEN" {
		t.Errorf("token_env = %q, want %q", got, "GITHUB_TOKEN")
	}
	if got := records.Count("without a token"); got != 0 {
		t.Errorf("an authenticated refusal logged the anonymous message; log = %q", records.Messages())
	}
}

// The engine answers every request in one refusal window with a copy of the
// same refusal, so a burst of failed jobs must produce one line, and a new
// window a new one. Serial: capture.Default mutates the global logger.
func TestGitHubLimitReporter_reportLogsEachRefusalWindowOnce(t *testing.T) {
	records := capture.Default(t)
	r := newGitHubLimitReporter("")
	now := time.UnixMilli(1791192600000).Add(-time.Hour)
	first := toolbelt.GitHubRateLimit{ResetAt: 1791192600000, Limit: 60}
	r.report(first, now)
	r.report(first, now)
	r.report(toolbelt.GitHubRateLimit{ResetAt: 1791196200000, Limit: 60}, now)

	if got := records.CountLevel(slog.LevelWarn, "tools: GitHub's rate limit"); got != 2 {
		t.Errorf("log = %q, want 2 rate-limit Warns (one per refusal window), got %d", records.Messages(), got)
	}
}

// The engine sends a credential's next GitHub request at its reset, or a minute
// after the refusal when that is later, so a refusal equal to the last one is a
// copy only before then and news after. Serial: capture.Default mutates the
// global logger.
func TestGitHubLimitReporter_reportLogsAnEqualRefusalAgainOnceTheEngineSendsAgain(t *testing.T) {
	refusedAt := time.Date(2026, 10, 5, 21, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		rl       toolbelt.GitHubRateLimit
		copyAt   time.Duration
		resentAt time.Duration
	}{
		"no reset time holds a minute": {
			rl: toolbelt.GitHubRateLimit{Limit: 60}, copyAt: 59 * time.Second, resentAt: 61 * time.Second,
		},
		"a reset time holds until it": {
			rl:     toolbelt.GitHubRateLimit{ResetAt: refusedAt.Add(30 * time.Minute).UnixMilli(), Limit: 60},
			copyAt: 10 * time.Minute, resentAt: 31 * time.Minute,
		},
	} {
		t.Run(name, func(t *testing.T) {
			records := capture.Default(t)
			r := newGitHubLimitReporter("")
			r.report(tc.rl, refusedAt)
			r.report(tc.rl, refusedAt.Add(tc.copyAt))
			if got := records.CountLevel(slog.LevelWarn, "tools: GitHub's rate limit"); got != 1 {
				t.Fatalf("report(%+v) at +0 and +%v: %d Warns, want 1 (the second is a copy); log = %q", tc.rl, tc.copyAt, got, records.Messages())
			}
			r.report(tc.rl, refusedAt.Add(tc.resentAt))
			if got := records.CountLevel(slog.LevelWarn, "tools: GitHub's rate limit"); got != 2 {
				t.Errorf("report(%+v) again at +%v: %d Warns, want 2 (GitHub refused a new request); log = %q", tc.rl, tc.resentAt, got, records.Messages())
			}
		})
	}
}

// A primary refusal can arrive with no reset time, and the line must then say
// how long the engine waits instead of pointing at an attribute it omitted.
// Serial: capture.Default mutates the global logger.
func TestGitHubLimitReporter_reportSaysHowLongItWaitsWithoutAResetTime(t *testing.T) {
	for name, tc := range map[string]struct {
		tokenEnv      envx.Key
		authenticated bool
		msg           string
	}{
		"with a token":    {tokenEnv: "GH_TOKEN", authenticated: true, msg: "tools: GitHub's rate limit for the configured token was reached"},
		"without a token": {msg: "tools: GitHub's rate limit for requests without a token was reached"},
	} {
		t.Run(name, func(t *testing.T) {
			records := capture.Default(t)
			r := newGitHubLimitReporter(tc.tokenEnv)
			r.report(toolbelt.GitHubRateLimit{Limit: 60, Authenticated: tc.authenticated}, time.Date(2026, 10, 5, 21, 0, 0, 0, time.UTC))

			if got, ok := records.AttrValue(tc.msg, "reset_at"); ok {
				t.Errorf("reset_at = %q for a refusal with no reset time, want it absent; log = %q", got, records.Messages())
			}
			if !records.AttrContains(tc.msg, "hint", "at least a minute") {
				t.Errorf("hint does not say the engine waits at least a minute; log = %q", records.Messages())
			}
			if records.AttrContains(tc.msg, "hint", "reset_at") {
				t.Errorf("hint names reset_at, which this line does not carry; log = %q", records.Messages())
			}
		})
	}
}

// run is what carries a refusal from the queue-lock callback to the log.
// Serial: capture.Default mutates the global logger.
func TestGitHubLimitReporter_runLogsWhatObserveQueued(t *testing.T) {
	records := capture.Default(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		r := newGitHubLimitReporter("")
		go r.run(ctx)
		r.observe(rateLimitedJob(toolbelt.GitHubRateLimit{ResetAt: 1791192600000}))
		synctest.Wait()
		if got := records.CountLevel(slog.LevelWarn, "tools: GitHub's rate limit"); got != 1 {
			t.Errorf("log = %q, want the queued refusal logged once, got %d", records.Messages(), got)
		}
		cancel()
		synctest.Wait()
	})
}
