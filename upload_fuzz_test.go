package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzUploadPartName: whatever Content-Disposition a client sends, an accepted name is a
// single plain ASCII path element that cannot leave the uploads root.
func FuzzUploadPartName(f *testing.F) {
	f.Add(`form-data; name="files"; filename="pasted-2026-08-15T08-42-11.png"`)
	f.Add(`form-data; name="files"; filename="pasted-2026-08-15T08-42-11-25.webp"`)
	f.Add(`form-data; name="files"; filename="../pasted-2026-08-15T08-42-11.png"`)
	f.Add(`form-data; name="files"; filename="pasted-2026-08-15T08-42-11.png\\..\\x"`)
	f.Add(`form-data; name="files"; filename*=UTF-8''pasted-2026-08-15T08-42-11%2F.png`)
	f.Add(`form-data; name="files"; filename*=UTF-8''pasted-2026-08-15T08-42-11.png%00`)
	f.Add(`form-data; name="file"; filename="pasted-2026-08-15T08-42-11.png"`)
	f.Add(`attachment; filename="pasted-2026-08-15T08-42-11.png"`)
	f.Fuzz(func(t *testing.T, disposition string) {
		name, err := uploadPartName(disposition)
		if err != nil {
			if name != "" {
				t.Fatalf("uploadPartName(%q) = %q with error %v, want no name on refusal", disposition, name, err)
			}
			return
		}
		if filepath.Base(name) != name || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") ||
			strings.HasPrefix(name, ".") || !filepath.IsLocal(name) {
			t.Fatalf("uploadPartName(%q) accepted %q, which is not one plain path element", disposition, name)
		}
		for _, r := range name {
			if r < 0x21 || r > 0x7e {
				t.Fatalf("uploadPartName(%q) accepted %q carrying rune %U", disposition, name, r)
			}
		}
		if !strings.HasPrefix(name, "pasted-") {
			t.Fatalf("uploadPartName(%q) accepted %q outside the pasted- namespace", disposition, name)
		}
	})
}
