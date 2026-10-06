package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/webhttp/v3"
)

const (
	pastedPNG  = "pasted-2026-08-15T08-42-11.png"
	pastedPNG2 = "pasted-2026-08-15T08-42-11-2.png"
	uploadURL  = "http://localhost:9848" + uploadsPath
)

// uploadPart is one multipart part: the raw Content-Disposition decides the field and
// the filename exactly as a client would send them.
type uploadPart struct {
	disposition string
	body        []byte
}

func filePart(name string, body []byte) uploadPart {
	// CreateFormFile's escaping, so a backslash or quote in name reaches the server as
	// the same name rather than a broken header.
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
	return uploadPart{disposition: `form-data; name="files"; filename="` + esc + `"`, body: body}
}

func multipartBody(t *testing.T, parts ...uploadPart) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	for _, p := range parts {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {p.disposition},
			"Content-Type":        {"image/png"},
		})
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if _, err := w.Write(p.body); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body, mw.FormDataContentType()
}

func uploadRequest(t *testing.T, parts ...uploadPart) *http.Request {
	t.Helper()
	body, ct := multipartBody(t, parts...)
	req := httptest.NewRequest(http.MethodPost, uploadURL, body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Origin", "http://localhost:9848")
	return req
}

// uploadChain is the production handler chain with uploads written under a fresh
// <tmp>/uploads, so a test can also prove nothing escaped into <tmp>.
func uploadChain(t *testing.T, hostPolicy *webhttp.HostPolicy) (h http.Handler, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "uploads")
	deps := newTestDeps(true)
	deps.uploadDir = dir
	mux, _, csp := mustRegisterRoutes(t, deps)
	return buildHandler(mux, nil, csp, hostPolicy), dir
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// filesUnder lists every regular file below root, relative to it.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Code
}

func TestUpload_writesPastedImagesAndReturnsTheirAbsolutePaths(t *testing.T) {
	h, dir := uploadChain(t, nil)
	first, second := []byte("\x89PNG first"), []byte("\x89PNG second")

	rec := serve(h, uploadRequest(t, filePart(pastedPNG, first), filePart(pastedPNG2, second)))

	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d %s, want 200", uploadsPath, rec.Code, rec.Body.String())
	}
	var got uploadBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	want := []string{filepath.Join(dir, pastedPNG), filepath.Join(dir, pastedPNG2)}
	if !slices.Equal(got.Uploaded, want) {
		t.Errorf("uploaded = %q, want %q", got.Uploaded, want)
	}
	for name, body := range map[string][]byte{pastedPNG: first, pastedPNG2: second} {
		b, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test temp dir
		if err != nil || !bytes.Equal(b, body) {
			t.Errorf("%s on disk = %q (err %v), want %q", name, b, err, body)
		}
	}
}

func TestUpload_refusesNamesOutsideThePastedImageAllowlist(t *testing.T) {
	names := []string{
		"../x.png",
		"a/b.png",
		`..\x.png`,
		".png",
		"pasted-x.png",
		"pasted-2026-08-15T08-42-11.exe",
		"pasted-2026-08-15T08-42-11.PNG",
		"pasted-2026-08-15T08-42-11.png/../../etc",
		// multipart.Part.FileName would Base this down to an accepted name.
		"../" + pastedPNG,
		"/uploads/" + pastedPNG,
		"",
	}
	for _, name := range names {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			h, dir := uploadChain(t, nil)
			rec := serve(h, uploadRequest(t, filePart(name, []byte("x"))))
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_filename" {
				t.Fatalf("filename %q = %d %s, want 400 invalid_filename", name, rec.Code, rec.Body.String())
			}
			if got := filesUnder(t, filepath.Dir(dir)); len(got) != 0 {
				t.Errorf("filename %q wrote %q, want nothing on disk", name, got)
			}
		})
	}
}

// RFC 2231 percent-encoding is the one route a control byte has into a header value.
func TestUpload_refusesControlBytesSmuggledThroughAnEncodedFilename(t *testing.T) {
	for _, enc := range []string{"pasted-2026-08-15T08-42-11%00.png", "pasted-2026-08-15T08-42-11.png%0D", "pasted-%E2%80%AE.png"} {
		t.Run(enc, func(t *testing.T) {
			h, dir := uploadChain(t, nil)
			part := uploadPart{disposition: `form-data; name="files"; filename*=UTF-8''` + enc, body: []byte("x")}
			rec := serve(h, uploadRequest(t, part))
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_filename" {
				t.Fatalf("filename*=%s = %d %s, want 400 invalid_filename", enc, rec.Code, rec.Body.String())
			}
			if got := filesUnder(t, filepath.Dir(dir)); len(got) != 0 {
				t.Errorf("filename*=%s wrote %q, want nothing on disk", enc, got)
			}
		})
	}
}

func TestUpload_refusesMalformedRequestsAsInvalidUpload(t *testing.T) {
	tooMany := make([]uploadPart, maxUploadFiles+1)
	for i := range tooMany {
		tooMany[i] = filePart(pastedPNG, []byte("x"))
	}
	cases := []struct {
		name string
		req  func(t *testing.T) *http.Request
	}{
		{"wrong field name", func(t *testing.T) *http.Request {
			return uploadRequest(t, uploadPart{disposition: `form-data; name="file"; filename="` + pastedPNG + `"`, body: []byte("x")})
		}},
		{"zero parts", func(t *testing.T) *http.Request { return uploadRequest(t) }},
		{"one part too many", func(t *testing.T) *http.Request { return uploadRequest(t, tooMany...) }},
		{"not multipart", func(t *testing.T) *http.Request {
			req := httptest.NewRequest(http.MethodPost, uploadURL, strings.NewReader(`{"files":[]}`))
			req.Header.Set("Content-Type", "application/json")
			return req
		}},
		{"malformed part header", func(t *testing.T) *http.Request {
			req := httptest.NewRequest(http.MethodPost, uploadURL, strings.NewReader("--b\r\nno colon here\r\n\r\nx\r\n--b--\r\n"))
			req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
			return req
		}},
		{"body ends inside a file", func(t *testing.T) *http.Request {
			body := "--b\r\nContent-Disposition: form-data; name=\"files\"; filename=\"" + pastedPNG + "\"\r\n\r\nabc"
			req := httptest.NewRequest(http.MethodPost, uploadURL, strings.NewReader(body))
			req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
			return req
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := uploadChain(t, nil)
			rec := serve(h, tc.req(t))
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_upload" {
				t.Fatalf("%s = %d %s, want 400 invalid_upload", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// A refused part does not roll back the parts before it; the client types nothing.
func TestUpload_partialBatchLeavesEarlierFiles(t *testing.T) {
	h, dir := uploadChain(t, nil)
	rec := serve(h, uploadRequest(t, filePart(pastedPNG, []byte("ok")), filePart("../evil.png", []byte("x"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := filesUnder(t, filepath.Dir(dir)); !slices.Equal(got, []string{filepath.Join("uploads", pastedPNG)}) {
		t.Errorf("files on disk = %q, want only the first, valid part", got)
	}
}

func TestUpload_refusesABodyOverTheCapWithNothingWritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	req := uploadRequest(t, filePart(pastedPNG, bytes.Repeat([]byte("a"), 4096)))

	rec := serve(handleUpload(dir, 1024), req)

	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "upload_too_large" {
		t.Fatalf("4 KiB body under a 1 KiB cap = %d %s, want 413 upload_too_large", rec.Code, rec.Body.String())
	}
	if got := filesUnder(t, filepath.Dir(dir)); len(got) != 0 {
		t.Errorf("an over-cap upload left %q on disk, want nothing", got)
	}
}

// The body cap, not the per-file one, is what stops many files that are each small.
func TestUpload_refusesManySmallFilesOverTheBodyCap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	small := bytes.Repeat([]byte("a"), 500)
	third := "pasted-2026-08-15T08-42-11-3.png"
	req := uploadRequest(t, filePart(pastedPNG, small), filePart(pastedPNG2, small), filePart(third, small))

	rec := serve(handleUpload(dir, 1024), req)

	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "upload_too_large" {
		t.Fatalf("three 500-byte files under a 1 KiB body cap = %d %s, want 413 upload_too_large", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, third)); !os.IsNotExist(err) {
		t.Errorf("the file that crossed the cap exists (stat err %v), want it absent", err)
	}
}

func TestUpload_isPOSTOnly(t *testing.T) {
	h, _ := uploadChain(t, nil)
	rec := serve(h, httptest.NewRequest(http.MethodGet, uploadURL, http.NoBody))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET %s = %d Allow=%q, want 405 Allow=POST", uploadsPath, rec.Code, rec.Header().Get("Allow"))
	}
}

// The endpoint has no auth of its own: it must inherit every refusal the terminal's own
// routes get from the middleware chain.
func TestUpload_inheritsTheCrossOriginAndHostGates(t *testing.T) {
	t.Run("cross-site fetch metadata", func(t *testing.T) {
		h, dir := uploadChain(t, nil)
		req := uploadRequest(t, filePart(pastedPNG, []byte("x")))
		req.Header.Del("Origin")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		if rec := serve(h, req); rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST = %d, want 403", rec.Code)
		}
		if got := filesUnder(t, filepath.Dir(dir)); len(got) != 0 {
			t.Errorf("a refused cross-site POST wrote %q", got)
		}
	})
	t.Run("foreign Origin", func(t *testing.T) {
		h, dir := uploadChain(t, nil)
		req := uploadRequest(t, filePart(pastedPNG, []byte("x")))
		req.Header.Set("Origin", "http://evil.example")
		if rec := serve(h, req); rec.Code != http.StatusForbidden {
			t.Errorf("foreign-Origin POST = %d, want 403", rec.Code)
		}
		if got := filesUnder(t, filepath.Dir(dir)); len(got) != 0 {
			t.Errorf("a refused foreign-Origin POST wrote %q", got)
		}
	})
	t.Run("host outside ALLOWED_HOSTS", func(t *testing.T) {
		t.Setenv("ALLOWED_HOSTS", "webterm.example.com")
		h, dir := uploadChain(t, parseAllowedHosts())
		body, ct := multipartBody(t, filePart(pastedPNG, []byte("x")))
		req := httptest.NewRequest(http.MethodPost, "http://evil.example:9848"+uploadsPath, body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Origin", "http://evil.example:9848")
		rec := serve(h, req)
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != "host_not_allowed" {
			t.Errorf("rebound-host POST = %d %s, want 403 host_not_allowed", rec.Code, rec.Body.String())
		}
		if got := filesUnder(t, filepath.Dir(dir)); len(got) != 0 {
			t.Errorf("a refused rebound-host POST wrote %q", got)
		}
	})
}

func TestUpload_unmountedWithoutAnUploadDir(t *testing.T) {
	mux, _, csp := mustRegisterRoutes(t, newTestDeps(true))
	rec := serve(buildHandler(mux, nil, csp, nil), uploadRequest(t, filePart(pastedPNG, []byte("x"))))
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "uploaded") {
		t.Fatalf("POST %s with no uploadDir = %d %s, want the route unmounted", uploadsPath, rec.Code, rec.Body.String())
	}
}

// The client mints names and checks responses against the same policy; a drift would
// make every paste fail at the server.
func TestUploadPolicyMatchesClient(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("static-src", "image-paste.ts"))
	if err != nil {
		t.Fatalf("read client policy: %v", err)
	}
	for _, want := range []string{
		`UPLOAD_PATH = "` + uploadsPath + `"`,
		`UPLOADS_DIR = "` + defaultUploadDir + `"`,
		"MAX_UPLOAD_BYTES = 256 * 1024 * 1024",
		"MAX_UPLOAD_FILES = 25",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("static-src/image-paste.ts lacks %q", want)
		}
	}
	if maxUploadSize != 256*1024*1024 || maxUploadFiles != 25 {
		t.Errorf("maxUploadSize=%d maxUploadFiles=%d, want the values the client literals above assume", maxUploadSize, maxUploadFiles)
	}
}
