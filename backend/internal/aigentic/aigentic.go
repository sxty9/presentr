// Package aigentic is presentr's thin client for aigentic's internal machine-to-machine run
// endpoint (POST internal/run, shared-secret auth). It lets presentr's backend run one AI turn ON
// BEHALF OF the calling user, grounded in the room's document pool — including uploaded PDFs and
// images, which aigentic reads natively (vision / document). The grounding assembly stays on the
// server so a file's bytes never round-trip out to the browser and back (Portionierte Daten); this
// client is the one place presentr speaks aigentic's contract, mirroring hosuto's peer client
// (Reuse before Build). The subject is named in the body; aigentic resolves it to a live OS identity
// and holds it to the same rights gates as the cookie-authed path, so presentr escalates no one. A
// disabled client (no URL or secret) leaves the room AI inert rather than failing the daemon — the
// same degrade-silently contract every sibling client keeps.
package aigentic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrDisabled means the client has no base URL or secret configured, so the M2M channel is off. The
// api layer maps it to a clear "assistant not configured" response and the UI degrades gracefully.
var ErrDisabled = errors.New("aigentic: client not configured")

// ErrUnavailable means aigentic turned the run away because no engine could serve it (503) — no
// local model reachable and no Claude credential linked for the subject. Actionable by an admin/user.
var ErrUnavailable = errors.New("aigentic: no AI engine is available")

// ErrNoVisionEngine means aigentic refused an image-bearing turn (422) because no reachable engine can
// SEE images — the request carried images but only a text-only local model is available. aigentic
// enforces this itself (an image never reaches a blind model that would fabricate a description); this
// client surfaces it as a distinct error so the caller can retry the turn without its images rather than
// fail. presentr only ever runs a single, un-nested turn, so a 422 from it is always this refusal.
var ErrNoVisionEngine = errors.New("aigentic: no vision-capable engine for an image request")

// InlineFile is one grounding part handed to aigentic without any server fs access: text carries its
// content directly; an image or PDF carries base64 bytes plus its media type (mirrors
// aigentic.InlineFile — the contract, never imported).
type InlineFile struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	MediaType string `json:"mediaType,omitempty"`
}

// Req is the subset of aigentic's request presentr sets: the prompt, the requested answer shape, an
// optional model override, and the room grounding as inline parts.
type Req struct {
	Prompt       string
	OutputFormat string // "text" | "markdown" | "json"
	Model        string
	Inline       []InlineFile
}

// Res is the subset of aigentic's result presentr renders and labels (Kennzeichnungspflicht).
type Res struct {
	Output string
	Engine string
	Model  string
}

// Client posts to aigentic's internal/run endpoint with the shared internal secret.
type Client struct {
	base   string
	secret string
	http   *http.Client
}

// New builds a client. baseURL is e.g. http://127.0.0.1:8780; secret is aigentic's shared internal
// secret. An empty base URL or secret leaves the client disabled (Run returns ErrDisabled). The
// timeout is generous — a grounded claude turn can take tens of seconds — and the caller further
// bounds it with the request context.
func New(baseURL, secret string) *Client {
	return &Client{
		base:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		secret: strings.TrimSpace(secret),
		http:   &http.Client{Timeout: 5 * time.Minute},
	}
}

// Enabled reports whether the client is configured to run turns.
func (c *Client) Enabled() bool { return c.base != "" && c.secret != "" }

// RequestLimit asks aigentic for the maximum RAW bytes one request may carry — the per-request ceiling
// aigentic OWNS and publishes as part of its interface. presentr uses it to size the sections a large
// file is split into, so the boundary is ERFRAGT, not guessed: if aigentic raises or lowers its ceiling,
// presentr follows with no code change (the Schnittstellen-Axiom). The value is read from the
// `maxRequestBytes` field of aigentic's info endpoint. ok is false when the client is disabled, the
// query cannot be made, or aigentic does not publish the field (an older build) — the caller then falls
// back to a conservative floor rather than failing.
func (c *Client) RequestLimit(ctx context.Context) (int64, bool) {
	if !c.Enabled() {
		return 0, false
	}
	endpoint := c.base + "/api/services/aigentic/info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("X-Aigentic-Internal-Secret", c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out struct {
		MaxRequestBytes int64 `json:"maxRequestBytes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, false
	}
	if out.MaxRequestBytes <= 0 {
		return 0, false
	}
	return out.MaxRequestBytes, true
}

// wireData mirrors the fields of aigentic.Request presentr sets, marshaled into the opaque prizm
// Data the endpoint decodes for the chosen engine.
type wireData struct {
	Prompt       string       `json:"prompt"`
	OutputFormat string       `json:"outputFormat,omitempty"`
	Model        string       `json:"model,omitempty"`
	Inline       []InlineFile `json:"inline,omitempty"`
}

// wireReq is the internal/run body: {subject, header, data}. Only header.kind matters for routing;
// "choose" is the Ask-AI standard (aigentic picks the best engine it can offer). aigentic stamps the
// authoritative subject itself from the top-level field.
type wireReq struct {
	Subject string `json:"subject"`
	Header  struct {
		Kind string `json:"kind"`
	} `json:"header"`
	Data json.RawMessage `json:"data"`
}

// Run executes one turn on behalf of subject and returns the engine's answer. It uses aigentic's
// `choose` router (the "Ask AI" standard: aigentic picks the best engine it can offer). A missing
// engine (aigentic 503) maps to ErrUnavailable; any other non-2xx to a generic error carrying
// aigentic's detail; a transport error (including the context deadline) is returned as-is.
func (c *Client) Run(ctx context.Context, subject string, req Req) (Res, error) {
	return c.dispatch(ctx, subject, "choose", wireData{
		Prompt: req.Prompt, OutputFormat: req.OutputFormat, Model: req.Model, Inline: req.Inline,
	})
}

// Extract reads the text CONTAINED in one file via aigentic's shared `extract` capability — the ONE
// place text recognition lives, so presentr never re-implements OCR (Reuse before Build). It reads a
// PDF's text layer AND the text inside its images, and any image's text (an equipment nameplate, a
// connector label), returning the transcription plus the engine/model that produced it
// (Kennzeichnungspflicht). path is provenance only; mediaType tells aigentic how to read the bytes
// (image → vision, application/pdf → document, text/* → inline text). Binary bytes ride base64; text
// rides inline, mirroring how the room grounding is assembled (one contract, no second encoding).
func (c *Client) Extract(ctx context.Context, subject, path, mediaType string, data []byte) (Res, error) {
	f := InlineFile{Path: path, MediaType: mediaType}
	if mediaType == "" || strings.HasPrefix(mediaType, "text/") {
		f.Content = string(data)
	} else {
		f.Content = base64.StdEncoding.EncodeToString(data)
	}
	return c.dispatch(ctx, subject, "extract", wireData{OutputFormat: "text", Inline: []InlineFile{f}})
}

// dispatch is the shared body of Run and Extract: it wraps wireData in the internal/run envelope under
// the given kind, presents the shared secret and the server-authoritative subject, and maps aigentic's
// outcome to a Res or a typed error. The kind selects aigentic's processor ("choose" | "extract").
func (c *Client) dispatch(ctx context.Context, subject, kind string, data wireData) (Res, error) {
	if !c.Enabled() {
		return Res{}, ErrDisabled
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Res{}, err
	}
	var body wireReq
	body.Subject = subject
	body.Header.Kind = kind
	body.Data = raw
	buf, err := json.Marshal(body)
	if err != nil {
		return Res{}, err
	}

	endpoint := c.base + "/api/services/aigentic/internal/run"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return Res{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Aigentic-Internal-Secret", c.secret)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Res{}, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return Res{}, fmt.Errorf("%w: %s", ErrUnavailable, detailOf(resp.Body))
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		// aigentic returns 422 for an image request it cannot serve with a vision-capable engine. presentr
		// runs only single, un-nested turns, so a 422 is always that refusal (never a recursion-depth limit).
		return Res{}, fmt.Errorf("%w: %s", ErrNoVisionEngine, detailOf(resp.Body))
	}
	if resp.StatusCode >= 300 {
		return Res{}, fmt.Errorf("aigentic: run failed (%d): %s", resp.StatusCode, detailOf(resp.Body))
	}

	var out struct {
		Data struct {
			Output string `json:"output"`
			Engine string `json:"engine"`
			Model  string `json:"model"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Res{}, fmt.Errorf("aigentic: bad response: %w", err)
	}
	return Res{Output: out.Data.Output, Engine: out.Data.Engine, Model: out.Data.Model}, nil
}

// detailOf pulls the {"detail": …} message out of an aigentic error body, falling back to the raw
// bytes when the body is not that shape.
func detailOf(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	var e struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(b, &e) == nil && e.Detail != "" {
		return e.Detail
	}
	return strings.TrimSpace(string(b))
}
