package api

// File uploads into the room's document pool. A document is knowledge the room's AI can draw on, so
// presentr accepts exactly the file kinds aigentic can actually read as model context — images
// (vision), PDFs (document), and text — and rejects anything else at the door WITH a reason, rather
// than storing an item the assistant could never use. Every accepted file is authored into a
// complete record HERE (identity, kind, mime, size, author, time) and handed to the passive pool;
// the bytes ride out of band via AddFile. Reading them back for a preview goes through getRaw.

import (
	"bytes"
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
	// maxFileBytes is THE upload limit: the most one file may weigh — 100 MB, big enough for a full
	// device manual or a floor-plan PDF, the very knowledge the room pool exists to hold. It is NO
	// LONGER coupled to aigentic's per-request ceiling: a file this large is stored in full and served
	// in full, and the AI grounding takes only as much of the pool as fits aigentic's request (see
	// ask.go, which now reports which documents it could and could not fully consider). The bytes are
	// streamed to disk as they arrive (AddFile), so a 100 MB upload never sits whole in the daemon's
	// memory. The UI names this limit at the entry point and turns an over-limit file away before a
	// byte is sent (a convenience); the server stays the authority and re-checks every file.
	maxFileBytes   = 100 << 20
	maxUploadFiles = 20
	// maxUploadBody FOLLOWS from the per-file limit and the file count (one number, not two): the
	// largest legitimate batch is maxUploadFiles files each at maxFileBytes, plus a small allowance
	// for the multipart envelope (boundaries/headers). It is only a last-resort backstop: the parts
	// are read as a stream, so an over-limit single file is caught by the per-file limit (a NAMED
	// rejection) long before this whole-body ceiling, and a body larger than any legitimate batch
	// answers a named 413 rather than an abrupt close. Nothing is buffered to reach these limits.
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
	// Read the multipart body as a STREAM (not ParseMultipartForm, which would buffer every part in
	// memory or spill it to a temp file first): each part's bytes flow straight into the pool as they
	// arrive, so a 100 MB file never sits whole in the daemon's memory.
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Could not read the uploaded files")
		return
	}

	type rejection struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	accepted := make([]store.Document, 0)
	rejected := make([]rejection, 0)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A body past the whole-batch ceiling trips the reader mid-stream. Answer with a named
			// 413 so the rejection ARRIVES as a readable response, not a torn stream. (The UI turns an
			// over-limit file away before sending, so a real user never reaches this backstop.)
			if isBodyTooLarge(err) {
				writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
					"This upload is too large — each file may be up to %d MB, and at most %d files at once", maxFileBytes>>20, maxUploadFiles))
				return
			}
			writeErr(w, http.StatusBadRequest, "Could not read the uploaded files")
			return
		}
		if part.FormName() != "files" {
			part.Close()
			continue
		}
		name := cleanName(part.FileName())
		if len(accepted)+len(rejected) >= maxUploadFiles {
			// Already at the per-request file cap — name the overflow rather than aborting the batch
			// (the files already stored stay stored). The UI uploads one file per request, so this is
			// only reached by a hand-built batch.
			part.Close()
			rejected = append(rejected, rejection{name, fmt.Sprintf("skipped — at most %d files per upload", maxUploadFiles)})
			continue
		}

		// Peek the first bytes to classify the file by its content (the same 512-byte window
		// http.DetectContentType uses), then stream the peek + the rest into the pool without ever
		// holding the whole file in memory.
		head := make([]byte, 512)
		hn, herr := io.ReadFull(part, head)
		if herr != nil && herr != io.EOF && herr != io.ErrUnexpectedEOF {
			part.Close()
			if isBodyTooLarge(herr) {
				writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
					"This upload is too large — each file may be up to %d MB, and at most %d files at once", maxFileBytes>>20, maxUploadFiles))
				return
			}
			rejected = append(rejected, rejection{name, "could not be read"})
			continue
		}
		head = head[:hn]
		if hn == 0 {
			part.Close()
			rejected = append(rejected, rejection{name, "the file is empty"})
			continue
		}
		mime, ok, reason := classifyUpload(name, head)
		if !ok {
			part.Close()
			rejected = append(rejected, rejection{name, reason})
			continue
		}
		// Author the complete record outside the pool, then stream the bytes over for passive
		// storage; the pool stamps Size from the bytes it writes and reports an over-limit file.
		d := store.Document{
			ID:      store.NewID(),
			Title:   name,
			Kind:    "file",
			Mime:    mime,
			Author:  u.Username,
			Created: time.Now().Unix(),
		}
		n, err := s.docs.AddFile(d, io.MultiReader(bytes.NewReader(head), part), maxFileBytes)
		part.Close()
		switch {
		case errors.Is(err, store.ErrFileTooLarge):
			rejected = append(rejected, rejection{name, fmt.Sprintf("is over the %d MB limit for one file", maxFileBytes>>20)})
			continue
		case isBodyTooLarge(err):
			writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"This upload is too large — each file may be up to %d MB, and at most %d files at once", maxFileBytes>>20, maxUploadFiles))
			return
		case err != nil:
			rejected = append(rejected, rejection{name, "could not be saved"})
			continue
		}
		d.Size = n
		accepted = append(accepted, d)
	}

	if len(accepted) == 0 && len(rejected) == 0 {
		writeErr(w, http.StatusBadRequest, "No files were uploaded")
		return
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

// isBodyTooLarge reports whether err is (or wraps) the MaxBytesReader's over-limit error, which the
// whole-batch ceiling raises mid-stream. It is answered with a named 413, not a torn stream.
func isBodyTooLarge(err error) bool {
	var e *http.MaxBytesError
	return errors.As(err, &e)
}

// getRaw streams a file document's raw bytes so the SDK viewers can render it (an image as an image,
// a PDF as a readable PDF). It STREAMS the blob straight from storage via http.ServeContent rather
// than buffering it, so serving a 100 MB file costs no matching chunk of memory, and range requests
// (a PDF viewer seeking, a video scrubbing) are answered from the seekable blob. Read-only and
// right-gated; text documents carry no bytes and 404 here.
func (s *Server) getRaw(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	id := r.PathValue("id")
	d, ok := s.docs.Get(id)
	if !ok || d.Kind != "file" {
		writeErr(w, http.StatusNotFound, "File not found")
		return
	}
	rc, _, ok := s.docs.OpenBlob(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "File not found")
		return
	}
	defer rc.Close()
	mime := d.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	// nosniff: serve exactly the stored type, so an uploaded file can never be reinterpreted as
	// active content by the browser. ServeContent honours this Content-Type (it only sniffs when the
	// header is unset).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline; filename=\""+sanitizeHeaderFilename(d.Title)+"\"")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, d.Title, time.Unix(d.Created, 0), rc)
}

// classifyUpload decides the stored mime of an upload and whether presentr accepts it, from the
// file's leading bytes (the sniff window) and its filename. It accepts only what aigentic can read
// as context: the four raster image formats (vision), PDF (document), and text (by sniff or by
// valid-UTF-8 content). Everything else is rejected with a reason naming the detected type. Only the
// head is examined — the standard 512-byte content-sniff window — so it works on a streamed upload
// whose full bytes are never held at once (data is already the peeked head).
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
