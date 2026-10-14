// Package embed talks to an EmbeddingGemma 2 server over the OpenAI
// /v1/embeddings protocol and holds the vector helpers the semantic index
// uses. Two backends work:
//
//   - the bundled sidecar (embed_server.py, `modon embed-server`), which also
//     embeds images: input items may be {"text", "image"} objects;
//   - any OpenAI-compatible text server (llama-server --embeddings, vLLM,
//     LM Studio, Ollama), which covers text only.
//
// The client applies EmbeddingGemma's task prefixes itself, so plain text
// servers get correctly prompted input too.
package embed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultModel is the model id recorded with stored vectors.
const DefaultModel = "google/embeddinggemma-2"

// ErrNoVision is returned when an image is embedded against a text-only
// server.
var ErrNoVision = errors.New("embedding server has no vision encoder (use the bundled sidecar: modon embed-server)")

// Task prefixes from the EmbeddingGemma 2 model card. Images take none.
const prefixQuery = "task: search result | query: "

// Query formats a search query.
func Query(q string) string { return prefixQuery + strings.TrimSpace(q) }

// Document formats an indexed document; title "none" when unknown.
func Document(title, text string) string {
	title = strings.TrimSpace(strings.ReplaceAll(title, "|", "/"))
	if title == "" {
		title = "none"
	}
	return "title: " + title + " | text: " + strings.TrimSpace(text)
}

// Input is one item to embed: text, an image, or both (text then image).
type Input struct {
	Text  string
	Image string // local path, http(s) URL or data: URL
}

// Info describes the server behind the endpoint.
type Info struct {
	Model           string `json:"model"`
	Vision          bool   `json:"vision"`
	Dims            int    `json:"dims"`
	Device          string `json:"device,omitempty"`
	PreferredDevice string `json:"preferred_device,omitempty"`
	Dtype           string `json:"dtype,omitempty"`
	OOMEvents       int    `json:"oom_events,omitempty"`
}

// Health-check timing: a successful probe or request is trusted for
// healthyTTL, a failed one is not retried for failedTTL, and server info is
// re-read after infoTTL so a restarted server (new device, vision on/off)
// is noticed.
var (
	healthyTTL   = 10 * time.Second
	failedTTL    = 3 * time.Second
	infoTTL      = 60 * time.Second
	probeTimeout = 3 * time.Second
)

// Client embeds through one endpoint at a fixed output dimension.
type Client struct {
	Endpoint string
	Model    string
	Dims     int
	http     *http.Client

	mu       sync.Mutex
	info     *Info
	infoAt   time.Time
	lastOK   time.Time
	lastFail time.Time
	lastErr  error
}

// New returns a client. dims is the Matryoshka truncation (768/512/256/128).
func New(endpoint, model string, dims int, timeout time.Duration) *Client {
	if model == "" {
		model = DefaultModel
	}
	if dims <= 0 {
		dims = 256
	}
	return &Client{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Model:    model,
		Dims:     dims,
		http:     &http.Client{Timeout: timeout},
	}
}

// StatusError is a non-200 answer from the server.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("embedding server: %d %s: %s", e.Code, http.StatusText(e.Code), e.Msg)
}

// InputError is a problem with one input (e.g. an unreadable image); it is
// never worth retrying.
type InputError struct {
	Index int
	Err   error
}

func (e *InputError) Error() string { return fmt.Sprintf("input %d: %v", e.Index, e.Err) }
func (e *InputError) Unwrap() error { return e.Err }

// Transient reports errors worth retrying: the server is down, restarting,
// timed out, or ran out of GPU memory (503). Bad input (400) is permanent.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusServiceUnavailable || se.Code == http.StatusBadGateway ||
			se.Code == http.StatusGatewayTimeout || se.Code == http.StatusTooManyRequests ||
			se.Code == http.StatusInternalServerError
	}
	var ie *InputError
	return !errors.As(err, &ie) && !errors.Is(err, ErrNoVision) && !errors.Is(err, context.Canceled)
}

// Unreachable reports errors meaning nothing answered at the endpoint
// (as opposed to the server answering with an error).
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	return !errors.As(err, &se) && Transient(err)
}

func (c *Client) markOK() {
	c.mu.Lock()
	c.lastOK, c.lastErr = time.Now(), nil
	c.mu.Unlock()
}

func (c *Client) markFailed(err error) {
	c.mu.Lock()
	c.lastOK, c.lastFail, c.lastErr = time.Time{}, time.Now(), err
	c.info = nil // re-read once it answers again
	c.mu.Unlock()
}

// Healthy reports whether the server answers, probing GET /modon/info at
// most once per healthyTTL while it is up and once per failedTTL while it
// is down. Embedding requests count as probes.
func (c *Client) Healthy(ctx context.Context) error {
	c.mu.Lock()
	switch {
	case time.Since(c.lastOK) < healthyTTL:
		c.mu.Unlock()
		return nil
	case c.lastErr != nil && time.Since(c.lastFail) < failedTTL:
		err := c.lastErr
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	_, err := c.probe(ctx)
	return err
}

// LastError is the most recent failure, nil after a success.
func (c *Client) LastError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// Info returns the server description, re-reading it after infoTTL.
// OpenAI-compatible servers without /modon/info are treated as text-only.
func (c *Client) Info(ctx context.Context) (Info, error) {
	c.mu.Lock()
	if c.info != nil && time.Since(c.infoAt) < infoTTL {
		info := *c.info
		c.mu.Unlock()
		return info, nil
	}
	c.mu.Unlock()
	return c.probe(ctx)
}

func (c *Client) probe(ctx context.Context) (Info, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, c.Endpoint+"/modon/info", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.markFailed(err)
		return Info{}, err
	}
	defer resp.Body.Close()
	info := Info{Model: c.Model}
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			err = fmt.Errorf("embedding server info: %w", err)
			c.markFailed(err)
			return Info{}, err
		}
	case resp.StatusCode >= 500:
		err := &StatusError{resp.StatusCode, "info"}
		c.markFailed(err)
		return Info{}, err
	}
	// Vectors are keyed by the configured model id so switching servers for
	// the same weights does not invalidate the index.
	info.Model = c.Model
	c.mu.Lock()
	c.info, c.infoAt = &info, time.Now()
	c.lastOK, c.lastErr = time.Now(), nil
	c.mu.Unlock()
	return info, nil
}

// Reset forgets the probed server state (e.g. after the sidecar restarts).
func (c *Client) Reset() {
	c.mu.Lock()
	c.info, c.lastOK, c.lastFail, c.lastErr = nil, time.Time{}, time.Time{}, nil
	c.mu.Unlock()
}

// Ping probes the server now, bypassing the health cache.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.probe(ctx)
	return err
}

type embedRequest struct {
	Model          string `json:"model"`
	Input          []any  `json:"input"`
	Dimensions     int    `json:"dimensions,omitempty"`
	EncodingFormat string `json:"encoding_format"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error any `json:"error"`
}

// Embed returns one L2-normalized vector of c.Dims per input. Images are read
// locally and sent as data URLs so the server need not share a filesystem.
func (c *Client) Embed(ctx context.Context, inputs []Input) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]any, len(inputs))
	for i, in := range inputs {
		if in.Image == "" {
			items[i] = in.Text
			continue
		}
		if !info.Vision {
			return nil, ErrNoVision
		}
		img, err := imageRef(in.Image)
		if err != nil {
			return nil, &InputError{Index: i, Err: err}
		}
		items[i] = map[string]string{"text": in.Text, "image": img}
	}
	body, _ := json.Marshal(embedRequest{Model: c.Model, Input: items, Dimensions: c.Dims, EncodingFormat: "float"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.markFailed(err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &body) == nil && body.Error.Message != "" {
			msg = body.Error.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		// The server answered, so it is alive even when it refuses work.
		c.markOK()
		return nil, &StatusError{resp.StatusCode, msg}
	}
	c.markOK()
	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("embedding server: %w", err)
	}
	if len(er.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding server returned %d vectors for %d inputs", len(er.Data), len(inputs))
	}
	out := make([][]float32, len(inputs))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embedding server: bad index %d", d.Index)
		}
		// Servers that ignore "dimensions" (llama-server) return 768; MRL
		// lets us truncate and re-normalize client-side.
		v, err := Fit(d.Embedding, c.Dims)
		if err != nil {
			return nil, err
		}
		out[d.Index] = v
	}
	return out, nil
}

// EmbedOne embeds a single input.
func (c *Client) EmbedOne(ctx context.Context, in Input) ([]float32, error) {
	v, err := c.Embed(ctx, []Input{in})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func imageRef(ref string) (string, error) {
	if strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref, nil
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return "", err
	}
	return DataURL(data, mimeFor(ref)), nil
}

// DataURL encodes image bytes for the sidecar.
func DataURL(data []byte, mime string) string {
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func mimeFor(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	}
	return ""
}

// ---- vector helpers ----

// Fit truncates v to dims (Matryoshka) and L2-normalizes it.
func Fit(v []float32, dims int) ([]float32, error) {
	if len(v) < dims {
		return nil, fmt.Errorf("embedding has %d dims, want %d", len(v), dims)
	}
	out := make([]float32, dims)
	copy(out, v[:dims])
	var n float64
	for _, x := range out {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, errors.New("embedding contains NaN/Inf (float16 inference?)")
		}
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return nil, errors.New("embedding is all zeros")
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range out {
		out[i] *= inv
	}
	return out, nil
}

// Dot is cosine similarity for normalized vectors.
func Dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}
