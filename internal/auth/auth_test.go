package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestLineRing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		halfCap    int
		perLineCap int
		lines      []string
		want       []string
	}{
		{"empty", 3, 128, nil, nil},
		{"under_half_cap", 3, 128, []string{"a", "b"}, []string{"a", "b"}},
		{"exactly_at_cap", 3, 128, []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"over_cap_rotation", 2, 128,
			[]string{"1", "2", "3", "4", "5"},
			[]string{"1", "2", "4", "5"}},
		{"truncation", 2, 4,
			[]string{"hello_world", "ab"},
			[]string{"hell", "ab"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newLineRing(tc.halfCap, tc.perLineCap)
			for _, l := range tc.lines {
				r.Push(l)
			}
			got := r.Sample()
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Sample() len = %d, want %d\ngot:  %v\nwant: %v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Sample()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLimitedWriter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		cap    int64
		writes []string
		want   string
		wantN  []int // expected return values from Write
	}{
		{"under_cap", 10, []string{"hello"}, "hello", []int{5}},
		{"exactly_at_cap", 5, []string{"hello"}, "hello", []int{5}},
		{"over_cap_single", 3, []string{"hello"}, "hel", []int{3}},
		{"multi_writes_past_cap", 5, []string{"abc", "defgh"}, "abcde", []int{3, 2}},
		{"zero_cap", 0, []string{"hello"}, "", []int{5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			lw := &limitedWriter{W: &buf, N: tc.cap}
			for i, w := range tc.writes {
				n, err := lw.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) error: %v", w, err)
				}
				if n != tc.wantN[i] {
					t.Errorf("Write(%q) = %d, want %d", w, n, tc.wantN[i])
				}
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("buf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractAuthURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
		want string
	}{
		{"empty", "", ""},
		{"no_url", "hello world", ""},
		{"bare_https", "https://example.com/auth?code=123", "https://example.com/auth?code=123"},
		{"open_this_url_prefix", "Open this URL: https://device.sso.us-east-1.amazonaws.com/auth", "https://device.sso.us-east-1.amazonaws.com/auth"},
		{"open_this_url_extra_words", "Open this URL: Visit https://example.com/login now", "https://example.com/login"},
		{"http_scheme_rejected", "Open this URL: http://evil.com/phish", ""},
		{"multiple_words_with_url", "Please visit https://auth.example.com/device to continue", "https://auth.example.com/device"},
		{"no_https_anywhere", "Visit http://insecure.example.com", ""},
		{"url_in_middle", "foo bar https://x.com/y baz", "https://x.com/y"},
		{"open_prefix_no_url", "Open this URL: not-a-url", ""},
		{"ftp_scheme", "ftp://files.example.com", ""},
		{"https_substring_not_prefix", "nothttps://fake.com", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractAuthURL(tc.line)
			if got != tc.want {
				t.Errorf("extractAuthURL(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestWhoamiInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(t *testing.T, m map[string]any)
	}{
		{"valid_snake_case", `{"account_type":"builderid","email":"u@x.com"}`, false, func(t *testing.T, m map[string]any) {
			t.Helper()
			if m["auth"] != authBuilderID {
				t.Errorf("auth = %v, want %q", m["auth"], authBuilderID)
			}
			if m["email"] != "u@x.com" {
				t.Errorf("email = %v, want u@x.com", m["email"])
			}
		}},
		{"valid_camel_case", `{"accountType":"identitycenter"}`, false, func(t *testing.T, m map[string]any) {
			t.Helper()
			if m["auth"] != authIdentityCenter {
				t.Errorf("auth = %v, want %q", m["auth"], authIdentityCenter)
			}
		}},
		{"null_json", `null`, false, func(t *testing.T, m map[string]any) {
			t.Helper()
			if m == nil {
				t.Error("expected non-nil map for null JSON")
			}
		}},
		{"empty_object", `{}`, false, func(t *testing.T, m map[string]any) {
			t.Helper()
			if _, ok := m["auth"]; ok {
				t.Error("auth should not be set for empty object")
			}
		}},
		{"trailing_non_json", `{"account_type":"social"}` + "\nProfile: default", false, func(t *testing.T, m map[string]any) {
			t.Helper()
			if m["auth"] != authSocialLogin {
				t.Errorf("auth = %v, want %q", m["auth"], authSocialLogin)
			}
		}},
		{"email_capitalized", `{"Email":"A@B.com"}`, false, func(t *testing.T, m map[string]any) {
			t.Helper()
			if m["email"] != "A@B.com" {
				t.Errorf("email = %v, want A@B.com", m["email"])
			}
		}},
		{"invalid_json", `not json`, true, nil},
		{"empty_input", ``, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := whoamiInfo([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func TestValidateProvider(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty_allowed", "", false},
		{"valid_https", "https://identity.example.com/start", false},
		{"http_rejected", "http://identity.example.com", true},
		{"no_scheme", "identity.example.com", true},
		{"too_long", "https://x.com/" + strings.Repeat("a", 2048), true},
		{"has_userinfo", "https://user:pass@example.com", true},
		{"ftp_scheme", "ftp://example.com", true},
		{"no_host", "https://", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateProvider(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateProvider(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateRegion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty_allowed", "", false},
		{"us_east_1", "us-east-1", false},
		{"cn_north_1", "cn-north-1", false},
		{"us_gov_west_1", "us-gov-west-1", false},
		{"us_iso_east_1", "us-iso-east-1", false},
		{"eu_isoe_west_1", "eu-isoe-west-1", false},
		{"uppercase", "US-EAST-1", true},
		{"shell_metachar", "us-east-1;rm", true},
		{"flag_smuggling", "--help", true},
		{"too_long", strings.Repeat("a", 33), true},
		{"empty_segment", "us--east-1", true},
		{"no_digit_suffix", "us-east-x", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateRegion(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateRegion(%q) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestBuildLoginArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider string
		region   string
		want     []string
	}{
		{"defaults", "", "", []string{"login", "--use-device-flow"}},
		{"provider_only", "https://id.example.com", "", []string{"login", "--use-device-flow", "--identity-provider", "https://id.example.com"}},
		{"region_only", "", "us-east-1", []string{"login", "--use-device-flow", "--region", "us-east-1"}},
		{"both", "https://id.example.com", "eu-west-1", []string{"login", "--use-device-flow", "--identity-provider", "https://id.example.com", "--region", "eu-west-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildLoginArgs(tc.provider, tc.region)
			if len(got) != len(tc.want) {
				t.Fatalf("buildLoginArgs(%q, %q) = %v, want %v", tc.provider, tc.region, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestHumanizeAccountType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"builderid", "builderid", authBuilderID},
		{"BuilderID_case", "BuilderID", authBuilderID},
		{"identitycenter", "identitycenter", authIdentityCenter},
		{"iamidentitycenter", "iamidentitycenter", authIdentityCenter},
		{"social", "social", authSocialLogin},
		{"unknown", "enterprise", authPrefixGeneric + "enterprise"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := humanizeAccountType(tc.input)
			if got != tc.want {
				t.Errorf("humanizeAccountType(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestScanLoginOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantURL  string
		wantCode string
		wantErr  string
	}{
		{"url_first_line", "https://auth.example.com/device\n", "https://auth.example.com/device", "", ""},
		{"open_this_url_prefix", "Open this URL: https://sso.aws/auth\n", "https://sso.aws/auth", "", ""},
		{"code_then_url", "Code: ABCD-EFGH\nOpen this URL: https://sso.aws/auth\n", "https://sso.aws/auth", "ABCD-EFGH", ""},
		{"already_logged_in", "You are already logged in\n", "", "", "already_logged_in"},
		{"eof_no_url", "some banner\nanother line\n", "", "", "no auth URL found in CLI output"},
		{"line_cap_exceeded", strings.Repeat("no url here\n", 201), "", "", "CLI produced too much output without auth URL"},
		{"url_with_ansi", "\x1b[1mOpen this URL: https://sso.aws/auth\x1b[0m\n", "https://sso.aws/auth", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ch := make(chan LoginScanResult, 1)
			scanLoginOutput(strings.NewReader(tc.input), ch)
			result := <-ch
			if tc.wantErr != "" {
				if result.Error != tc.wantErr {
					t.Errorf("error = %q, want %q", result.Error, tc.wantErr)
				}
				return
			}
			if result.URL != tc.wantURL {
				t.Errorf("url = %q, want %q", result.URL, tc.wantURL)
			}
			if result.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", result.Code, tc.wantCode)
			}
		})
	}
}
