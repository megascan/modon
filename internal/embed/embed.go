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
	Model  string `json:"model"`
	Vision bool   `json:"vision"`
	Dims   int    `json:"dims"`
	Device string `json:"device,omitempty"`
	Dtype  string `json:"dtype,omitempty"`
}

// Client embeds through one endpoint at a fixed output dimension.
type Client struct {
	Endpoint string
	Model    string
	Dims     int
	http     *http.Client

	mu   sync.Mutex
	info *Info
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

// Info probes the server once (GET /modon/info). OpenAI-compatible servers
// without that route are treated as text-only.
func (c *Client) Info(ctx context.Context) (Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.info != nil {
		return *c.info, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint+"/modon/info", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	info := Info{Model: c.Model}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			return Info{}, fmt.Errorf("embedding server info: %w", err)
		}
	}
	// Vectors are keyed by the configured model id so switching servers for
	// the same weights does not invalidate the index.
	info.Model = c.Model
	c.info = &info
	return info, nil
}

// Reset forgets the probed server info (e.g. after the sidecar restarts).
func (c *Client) Reset() {
	c.mu.Lock()
	c.info = nil
	c.mu.Unlock()
}

// Ping reports whether the server answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Info(ctx)
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
			return nil, err
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
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("embedding server: %s: %s", resp.Status, msg)
	}
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
