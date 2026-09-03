package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// shared HTTP plumbing for the remote sources.

func httpClient(timeoutSeconds int) *http.Client {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	return &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second}
}

const userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0 modon/0.1"

// httpGet performs a GET with browser-ish headers and returns the body.
func httpGet(ctx context.Context, c *http.Client, url string, hdr map[string]string) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, image/*, */*")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, resp.StatusCode, resp.Header, err
	}
	return body, resp.StatusCode, resp.Header, nil
}

func statusErr(what string, code int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	return fmt.Errorf("%s: HTTP %d: %s", what, code, snippet)
}

// readImage loads an image file and sniffs its mime type.
func readImage(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return data, sniffImage(data), nil
}

func sniffImage(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 3 && b[0] == 'G' && b[1] == 'I' && b[2] == 'F':
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}

// extForMime maps a mime type to a file extension for cached previews.
func extForMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".img"
	}
}
