package auth

import (
	"bytes"
	"testing"
)

func FuzzValidateProvider(f *testing.F) {
	f.Add("")
	f.Add("https://example.com")
	f.Add("https://sso.aws.amazon.com/start")
	f.Add("http://insecure.example.com")
	f.Add("ftp://files.example.com")
	f.Add("https://user:pass@example.com")
	f.Add("not-a-url")
	f.Add("https://")
	f.Fuzz(func(t *testing.T, s string) {
		err := validateProvider(s)
		if err == nil && s != "" {
			if len(s) > maxProviderLen {
				t.Errorf("validateProvider accepted too-long input: %d", len(s))
			}
		}
	})
}

func FuzzValidateRegion(f *testing.F) {
	f.Add("")
	f.Add("us-east-1")
	f.Add("eu-west-2")
	f.Add("cn-north-1")
	f.Add("us-gov-west-1")
	f.Add("us-iso-east-1")
	f.Add("--help")
	f.Add("us-east-1; rm -rf /")
	f.Add("UPPER-CASE-1")
	f.Fuzz(func(t *testing.T, s string) {
		err := validateRegion(s)
		if err == nil && s != "" {
			if !awsRegionRe.MatchString(s) {
				t.Errorf("validateRegion accepted non-matching region: %q", s)
			}
		}
	})
}

func FuzzLimitedWriter(f *testing.F) {
	f.Add([]byte("hello"), int64(10))
	f.Add([]byte("hello world"), int64(5))
	f.Add([]byte(""), int64(0))
	f.Add([]byte("data"), int64(0))
	f.Add([]byte("x"), int64(1))
	f.Fuzz(func(t *testing.T, data []byte, cap int64) {
		if cap < 0 {
			cap = 0
		}
		var buf bytes.Buffer
		lw := &limitedWriter{W: &buf, N: cap}
		_, _ = lw.Write(data)
		if int64(buf.Len()) > cap {
			t.Errorf("limitedWriter exceeded cap: wrote %d bytes with cap %d", buf.Len(), cap)
		}
	})
}
