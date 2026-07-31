package api

// File uploads into the room's document pool. A document is knowledge the room's AI can draw on, so
// presentr accepts exactly the file kinds aigentic can actually read as model context — images
// (vision), PDFs (document), and text — and rejects anything else at the door WITH a reason, rather
// than storing an item the assistant could never use. Every accepted file is authored into a
// complete record HERE (identity, kind, mime, size, author, time) and handed to the passive pool;
// the bytes ride out of band via AddFile. Reading them back for a preview goes through getRaw.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"presentr/internal/auth"
	"presentr/internal/store"
)

const (
	// maxFileBytes is THE upload limit: the most one file may weigh. It is deliberately coupled to
	// the grounding path, not free-standing — ask.go budgets maxGroundingBytes (24 MiB of raw bytes)
	// across the WHOLE pool, and that budget base64-inflates by 4/3 to ~32 MiB on the wire, exactly
	// aigentic's per-request ceiling. A file heavier than this could never be fully read as room
	// knowledge, so accepting it would be dishonest. Raising it would require aigentic's ceiling to
	// rise AND a chunking strategy for large PDFs — out of this task's scope. The UI names this limit
	// at the entry point and rejects an over-limit file before a byte is sent (a convenience); the
	// server stays the authority.
	maxFileBytes   = 20 << 20
	maxUploadFiles = 20
	// maxUploadBody FOLLOWS from the per-file limit and the file count (one number, not two): the
	// largest legitimate batch is maxUploadFiles files each at maxFileBytes, plus a small allowance
	// for the multipart envelope (boundaries/headers). Because it can never be smaller than a single
	// allowed file, a normally-oversized single file is parsed in full and then turned away with the
	// NAMED per-file reason below — the browser reads a complete response, not a stream the server
	// aborted mid-upload. The whole-body reader is thus a last-resort backstop against a body larger
	// than any legitimate batch, and that case answers a named 413 rather than an abrupt close.
	maxUploadBody = maxUploadFiles*maxFileBytes + (1 << 20)
)

// acceptedImages are the raster formats aigentic reads via vision. pdf and text are handled
// separately (pdf as a document; text by content).
var acceptedImages = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// uploadFiles ingests one multipart batch of files into the pool. Each part is classified; usable
// files are stored and unusable ones are reported back with a reason (never silently dropped and
// never stored as junk). A well-formed batch always answers 200 with what landed and what did not,
// so the UI can surface per-file outcomes uniformly.
func (s *Server) uploadFiles(w http.ResponseWriter, r *http.Request, u *auth.User) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		// A body larger than the largest legitimate batch trips the whole-body reader. Answer with a
		// named 413 so the rejection ARRIVES as a readable response document, instead of the generic
		// error that reads as a torn stream. (The UI rejects an over-limit file before sending, so a
		// real user never reaches this; it is the honest backstop for anything that slips past.)
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"This upload is too large — each file may be up to %d MB, and at most %d files at once", maxFileBytes>>20, maxUploadFiles))
			return
		}
		writeErr(w, http.StatusBadRequest, "Could not read the uploaded files")
		return
	}
	parts := r.MultipartForm.File["files"]
	if len(parts) == 0 {
		writeErr(w, http.StatusBadRequest, "No files were uploaded")
		return
	}
	if len(parts) > maxUploadFiles {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("Too many files at once — upload at most %d", maxUploadFiles))
		return
	}

	type rejection struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	accepted := make([]store.Document, 0, len(parts))
	rejected := make([]rejection, 0)

	for _, fh := range parts {
		name := cleanName(fh.Filename)
		if fh.Size == 0 {
			rejected = append(rejected, rejection{name, "the file is empty"})
			continue
		}
		if fh.Size > maxFileBytes {
			rejected = append(rejected, rejection{name, fmt.Sprintf("%d MB is over the %d MB limit for one file", fh.Size>>20, maxFileBytes>>20)})
			continue
		}
		f, err := fh.Open()
		if err != nil {
			rejected = append(rejected, rejection{name, "could not be read"})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		f.Close()
		if err != nil {
			rejected = append(rejected, rejection{name, "could not be read"})
			continue
		}
		if len(data) == 0 {
			rejected = append(rejected, rejection{name, "the file is empty"})
			continue
		}
		if len(data) > maxFileBytes {
			rejected = append(rejected, rejection{name, fmt.Sprintf("%d MB is over the %d MB limit for one file", len(data)>>20, maxFileBytes>>20)})
			continue
		}

		mime, ok, reason := classifyUpload(name, data)
		if !ok {
			rejected = append(rejected, rejection{name, reason})
			continue
		}
		// Author the complete record outside the pool, then hand the bytes over for passive storage.
		d := store.Document{
			ID:      store.NewID(),
			Title:   name,
			Kind:    "file",
			Mime:    mime,
			Size:    int64(len(data)),
			Author:  u.Username,
			Created: time.Now().Unix(),
		}
		if err := s.docs.AddFile(d, data); err != nil {
			rejected = append(rejected, rejection{name, "could not be saved"})
			continue
		}
		accepted = append(accepted, d)
	}

	if len(accepted) == 0 {
		// Nothing usable landed — surface the first reason as the error so a single-file upload
		// gets a clear rejection, while still carrying the full per-file list.
		detail := "None of the files could be used as room knowledge"
		if len(rejected) > 0 {
			detail = rejected[0].Name + ": " + rejected[0].Reason
		}
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"detail": detail, "rejected": rejected})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "documents": accepted, "rejected": rejected})
}

// getRaw streams a file document's raw bytes so the SDK viewers can render it (an image as an image,
// a PDF as a readable PDF). Read-only and right-gated; text documents carry no bytes and 404 here.
func (s *Server) getRaw(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	id := r.PathValue("id")
	d, ok := s.docs.Get(id)
	if !ok || d.Kind != "file" {
		writeErr(w, http.StatusNotFound, "File not found")
		return
	}
	b, ok := s.docs.Bytes(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "File not found")
		return
	}
	mime := d.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	// nosniff: serve exactly the stored type, so an uploaded file can never be reinterpreted as
	// active content by the browser.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeHeaderFilename(d.Title)+"\"")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// classifyUpload decides the stored mime of an upload and whether presentr accepts it, from the
// bytes (sniffed) and the filename. It accepts only what aigentic can read as context: the four
// raster image formats (vision), PDF (document), and text (by sniff or by valid-UTF-8 content).
// Everything else is rejected with a reason naming the detected type.
func classifyUpload(name string, data []byte) (mime string, ok bool, reason string) {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	sniff := strings.TrimSpace(strings.SplitN(http.DetectContentType(head), ";", 2)[0])

	switch {
	case acceptedImages[sniff]:
		return sniff, true, ""
	case sniff == "application/pdf":
		return "application/pdf", true, ""
	case strings.HasPrefix(sniff, "text/"):
		return textMime(name), true, ""
	case looksLikeText(data):
		// DetectContentType flags many code/config files as octet-stream; accept them as text when
		// the bytes are genuine UTF-8 text.
		return textMime(name), true, ""
	default:
		return "", false, fmt.Sprintf("files of type %s can't be read as room knowledge — upload an image, PDF, or text document", sniff)
	}
}

// textMime canonicalises a text file's stored type from its extension, so a markdown document
// previews as markdown while other text stays text/plain. aigentic reads any text/* as text.
func textMime(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".csv":
		return "text/csv"
	default:
		return "text/plain; charset=utf-8"
	}
}

// looksLikeText reports whether bytes are plausibly a text document: valid UTF-8 with no NUL byte.
// This is what separates a code/config file (accepted as text) from a binary blob (rejected).
func looksLikeText(data []byte) bool {
	if len(data) == 0 || !utf8.Valid(data) {
		return false
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}

// cleanName reduces an uploaded filename to a bare, bounded base name (no path, no control chars),
// so a crafted upload can neither traverse nor bloat a title.
func cleanName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "/" || name == "" {
		name = "file"
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	return name
}

// sanitizeHeaderFilename strips anything that could break out of the Content-Disposition quoted
// string (quotes, backslashes, control bytes).
func sanitizeHeaderFilename(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
}
