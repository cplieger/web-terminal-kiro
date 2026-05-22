package auth

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzExtractAuthURL(f *testing.F) {
	f.Add("")
	f.Add("https://example.com/auth")
	f.Add("Open this URL: https://sso.aws/device")
	f.Add("no url here")
	f.Add("http://insecure.example.com")
	f.Add("Open this URL: not-a-url")
	f.Fuzz(func(t *testing.T, line string) {
		result := extractAuthURL(line)
		if result != "" && !strings.HasPrefix(result, "https://") {
			t.Errorf("extractAuthURL returned non-https URL: %q", result)
		}
	})
}

func FuzzWhoamiInfo(f *testing.F) {
	f.Add([]byte(`{"account_type":"builderid","email":"u@x.com"}`))
	f.Add([]byte(`{"accountType":"identitycenter"}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := whoamiInfo(data)
		if err == nil && info == nil {
			t.Error("whoamiInfo returned nil map without error")
		}
	})
}

func FuzzScanLoginOutput(f *testing.F) {
	f.Add([]byte("https://auth.example.com/device\n"))
	f.Add([]byte("Open this URL: https://sso.aws/auth\n"))
	f.Add([]byte("Code: ABCD-EFGH\nOpen this URL: https://sso.aws/auth\n"))
	f.Add([]byte("You are already logged in\n"))
	f.Add([]byte("some banner\nanother line\n"))
	f.Add([]byte(strings.Repeat("no url here\n", 201)))
	f.Add([]byte("\x1b[1mOpen this URL: https://sso.aws/auth\x1b[0m\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		ch := make(chan LoginScanResult, 1)
		scanLoginOutput(bytes.NewReader(data), ch)
		<-ch
	})
}
