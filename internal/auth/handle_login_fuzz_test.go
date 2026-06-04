package auth

import (
	"net/url"
	"testing"
)

func FuzzValidateProvider(f *testing.F) {
	f.Add("")
	f.Add("https://sso.aws.amazon.com/start")
	f.Add("http://evil.com")
	f.Add("https://user:pass@evil.com/path")
	f.Fuzz(func(t *testing.T, v string) {
		err := validateProvider(v)
		if err == nil && v != "" {
			u, perr := url.Parse(v)
			if perr != nil || u.Scheme != "https" || u.Host == "" {
				t.Errorf("validateProvider accepted invalid URL: %q", v)
			}
			if u.User != nil {
				t.Errorf("validateProvider accepted URL with credentials: %q", v)
			}
			if len(v) > maxProviderLen {
				t.Errorf("validateProvider accepted oversized input: len=%d", len(v))
			}
		}
	})
}

func FuzzValidateRegion(f *testing.F) {
	f.Add("")
	f.Add("us-east-1")
	f.Add("--help")
	f.Add("us-gov-west-1")
	f.Fuzz(func(t *testing.T, v string) {
		err := validateRegion(v)
		if err == nil && v != "" {
			if !awsRegionRe.MatchString(v) {
				t.Errorf("validateRegion accepted non-matching region: %q", v)
			}
			if len(v) > maxRegionLen {
				t.Errorf("validateRegion accepted oversized region: len=%d", len(v))
			}
		}
	})
}
