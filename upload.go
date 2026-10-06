package main

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/cplieger/atomicfile/v4"
	"github.com/cplieger/webhttp/v3"
)

// Image paste uploads. The browser posts a clipboard image here and types the returned
// absolute path at the prompt, where kiro-cli attaches it on submit.
const (
	// defaultUploadDir is a literal container root, not derived from the workspace, so an
	// upload never lands in a repository checkout. static-src/image-paste.ts mirrors it.
	defaultUploadDir = "/uploads"
	// maxUploadSize bounds the whole request body and each file.
	maxUploadSize  = 256 << 20
	maxUploadFiles = 25
	// uploadReadTimeout bounds how long one upload may take to arrive: the server sets no
	// ReadTimeout because /ws and the SSE stream must stay open, so without this a slow
	// body could hold the request forever. Longer than the client's own 120 s abort.
	uploadReadTimeout = 3 * time.Minute
	// uploadField is the multipart field every file travels in.
	uploadField = "files"
)

// uploadNamePattern is the ONLY name a file may be written under: the client mints it,
// and the allowlist leaves no room for a separator, a dot-dot, a leading dot, a control
// byte or a non-ASCII rune.
var uploadNamePattern = regexp.MustCompile(`^pasted-[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}(-[0-9]{1,2})?\.(png|jpg|webp)$`)

// errInvalidUpload and errInvalidFilename are the two client-fault refusals.
var (
	errInvalidUpload   = errors.New("invalid upload")
	errInvalidFilename = errors.New("invalid filename")
)

// uploadBody is the success envelope: the absolute path of every file written, in the
// order the request carried them.
type uploadBody struct {
	Uploaded []string `json:"uploaded"`
}

// handleUpload streams a multipart body of pasted images into dir. A failure partway
// through leaves the earlier files on disk; the client types nothing on any failure.
func handleUpload(dir string, maxBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// ErrNotSupported (a recorder in tests) leaves the request unbounded in time only.
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout)); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			slog.Debug("image upload: read deadline not set", "error", err)
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		paths, total, err := writeUploads(r, dir, maxBytes)
		if err != nil {
			respondUploadError(w, r, err, maxBytes)
			return
		}
		names := make([]string, len(paths))
		for i, p := range paths {
			names[i] = filepath.Base(p)
		}
		slog.Info("image upload", "count", len(paths), "bytes", total, "names", names)
		webhttp.WriteJSON(w, uploadBody{Uploaded: paths})
	}
}

// writeUploads writes every part of r's multipart body into dir, refusing the first part
// that is not a well-named file in the expected field.
func writeUploads(r *http.Request, dir string, maxBytes int64) (paths []string, total int64, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, 0, errors.Join(errInvalidUpload, err)
	}
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return nil, 0, mkErr
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = root.Close() }()

	for {
		part, partErr := mr.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			// A malformed body is the client's fault; respondUploadError still tells an
			// oversize or timed-out read apart, since both stay reachable through the join.
			return paths, total, errors.Join(errInvalidUpload, partErr)
		}
		if len(paths) == maxUploadFiles {
			return paths, total, errInvalidUpload
		}
		name, nameErr := uploadPartName(part.Header.Get("Content-Disposition"))
		if nameErr != nil {
			return paths, total, nameErr
		}
		cr := &countingReader{r: part}
		if _, wErr := atomicfile.WriteReaderInRoot(r.Context(), root, name, cr,
			atomicfile.WithMaxBytes(maxBytes)); wErr != nil {
			return paths, total, wErr
		}
		paths = append(paths, filepath.Join(dir, name))
		total += cr.n
	}
	if len(paths) == 0 {
		return nil, 0, errInvalidUpload
	}
	return paths, total, nil
}

// uploadPartName validates a part's Content-Disposition and returns the RAW filename.
// multipart.Part.FileName is not used: it applies filepath.Base, which would turn
// "../pasted-….png" into an accepted name instead of a refusal.
func uploadPartName(disposition string) (string, error) {
	kind, params, err := mime.ParseMediaType(disposition)
	if err != nil || kind != "form-data" || params["name"] != uploadField {
		return "", errInvalidUpload
	}
	name := params["filename"]
	if !uploadNamePattern.MatchString(name) {
		return "", errInvalidFilename
	}
	return name, nil
}

// respondUploadError maps a writeUploads failure to its refusal. A client that walked
// away gets nothing written back.
func respondUploadError(w http.ResponseWriter, r *http.Request, err error, maxBytes int64) {
	if r.Context().Err() != nil {
		slog.Debug("image upload abandoned by the client", "error", err)
		return
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok || errors.Is(err, atomicfile.ErrFileTooLarge) {
		slog.Warn("image upload too large", "limit", maxBytes)
		webhttp.WriteError(w, r, http.StatusRequestEntityTooLarge, "upload_too_large", "image is too large to upload")
		return
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		slog.Warn("image upload timed out", "timeout", uploadReadTimeout)
		webhttp.WriteError(w, r, http.StatusRequestTimeout, "upload_timeout", "image upload took too long")
		return
	}
	if errors.Is(err, errInvalidFilename) {
		webhttp.WriteError(w, r, http.StatusBadRequest, "invalid_filename", "file name is not a pasted image name")
		return
	}
	// A body that ends inside a part is malformed, not a server fault.
	if errors.Is(err, errInvalidUpload) || errors.Is(err, io.ErrUnexpectedEOF) {
		webhttp.WriteError(w, r, http.StatusBadRequest, "invalid_upload",
			"expected a multipart body of 1 to 25 files in the \"files\" field")
		return
	}
	slog.Warn("image upload failed", "error", err)
	webhttp.WriteError(w, r, http.StatusInternalServerError, "upload_failed", "image upload failed")
}

// countingReader counts the bytes a write consumed, for the upload log line.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
