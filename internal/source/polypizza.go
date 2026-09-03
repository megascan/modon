package source

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modon/internal/config"
	"modon/internal/model"
)

// PolyPizza implements the api.poly.pizza v1.1 contract:
//
//	GET /search/{keyword}?category=&license=&animated=&limit=&page=
//	GET /model/{id}
//	header: X-Auth-Token: <api key>
type PolyPizza struct {
	cfg  *config.Config
	http *http.Client
	base string // overridable for tests
}

func NewPolyPizza(cfg *config.Config) *PolyPizza {
	return &PolyPizza{cfg: cfg, http: httpClient(cfg.HTTPTimeoutSeconds), base: "https://api.poly.pizza/v1.1"}
}

func (p *PolyPizza) Name() string    { return model.SourcePolyPizza }
func (p *PolyPizza) Available() bool { return p.cfg.PolyPizzaAPIKey != "" }

func (p *PolyPizza) headers() map[string]string {
	return map[string]string{"X-Auth-Token": p.cfg.PolyPizzaAPIKey}
}

// ppModel tolerates both the documented lowercase field names and the
// PascalCase variants the API has used historically.
type ppModel struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Attribution string `json:"attribution"`
	Thumbnail   string `json:"thumbnail"`
	Download    string `json:"download"`
	TriCount    int64  `json:"triCount"`
	Creator     struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"creator"`
	CreatorName string `json:"creatorName"`
	UploadDate  string `json:"uploadDate"`
	Category    any    `json:"category"`
	License     string `json:"license"`
	Animated    bool   `json:"animated"`
	URL         string `json:"url"`
}

type ppSearchResp struct {
	Total        int       `json:"total"`
	TotalResults int       `json:"totalResults"`
	Results      []ppModel `json:"results"`
	Models       []ppModel `json:"models"`
}

func (r *ppSearchResp) items() []ppModel {
	if len(r.Results) > 0 {
		return r.Results
	}
	return r.Models
}

func (m *ppModel) toAsset() model.Asset {
	creator := m.Creator.Name
	if creator == "" {
		creator = m.CreatorName
	}
	license := m.License
	if license == "" && m.Attribution != "" {
		license = "see attribution"
	}
	a := model.Asset{
		ID:          model.SourcePolyPizza + ":" + m.ID,
		Source:      model.SourcePolyPizza,
		Title:       m.Title,
		Description: m.Description,
		Creator:     creator,
		License:     license,
		AssetType:   "model",
		Animated:    m.Animated,
		TriCount:    m.TriCount,
		Formats:     []string{"glb", "gltf"},
		PreviewURL:  m.Thumbnail,
		DownloadURL: m.Download,
		URL:         m.URL,
		IsFree:      true,
		Owned:       true,
	}
	if a.URL == "" && m.ID != "" {
		a.URL = "https://poly.pizza/m/" + m.ID
	}
	if a.DownloadURL == "" && m.ID != "" {
		a.DownloadURL = "https://api.poly.pizza/v1.1/download/" + m.ID
	}
	if c, ok := m.Category.(string); ok {
		a.Category = c
		a.Tags = []string{c}
	} else if f, ok := m.Category.(float64); ok {
		a.Category = polyCategoryName(int(f))
		a.Tags = []string{a.Category}
	}
	return a
}

// polyCategoryName maps poly.pizza's numeric categories.
func polyCategoryName(c int) string {
	names := []string{"food-drink", "clutter", "weapons", "transport", "furniture-decor",
		"objects", "nature", "animals", "buildings-architecture", "people-characters",
		"scenes-levels", "other"}
	if c >= 0 && c < len(names) {
		return names[c]
	}
	return "other"
}

// Search queries poly.pizza by keyword.
func (p *PolyPizza) Search(ctx context.Context, q model.Query) ([]model.Asset, error) {
	if q.Text == "" && q.Category == "" && q.Style == "" && len(q.Tags) == 0 {
		return nil, nil
	}
	keyword := q.Text
	if keyword == "" && len(q.Tags) > 0 {
		keyword = strings.Join(q.Tags, " ")
	}
	if keyword == "" {
		keyword = q.Category
	}
	params := url.Values{}
	if q.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", min(q.Limit, 32)))
	}
	if q.Animated != nil && *q.Animated {
		params.Set("animated", "true")
	}
	path := "/search/" + url.PathEscape(keyword)
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	body, code, _, err := httpGet(ctx, p.http, p.base+path, p.headers())
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("poly.pizza search", code, body)
	}
	var resp ppSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("poly.pizza: unexpected response: %w", err)
	}
	out := make([]model.Asset, 0, len(resp.items()))
	for _, m := range resp.items() {
		a := m.toAsset()
		// Local style/category filtering in Go (the API has no style filter).
		if q.Category != "" && !containsFold(a.Category, q.Category) && !containsFold(strings.Join(a.Tags, " "), q.Category) && !containsFold(a.Title, q.Category) {
			continue
		}
		if q.Style != "" && !containsFold(a.Title+" "+a.Description+" "+strings.Join(a.Tags, " "), q.Style) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// Model fetches one model's details.
func (p *PolyPizza) Model(ctx context.Context, id string) (*model.Asset, error) {
	body, code, _, err := httpGet(ctx, p.http, p.base+"/model/"+url.PathEscape(id), p.headers())
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("poly.pizza model", code, body)
	}
	var m ppModel
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	a := m.toAsset()
	a.ID = model.SourcePolyPizza + ":" + id
	return &a, nil
}

func (p *PolyPizza) Details(ctx context.Context, id string) (*model.Asset, error) {
	return p.Model(ctx, id)
}

// Preview downloads the thumbnail to the cache and returns the bytes.
func (p *PolyPizza) Preview(ctx context.Context, a model.Asset) ([]byte, string, string, error) {
	if a.PreviewURL == "" {
		return nil, "", "", fmt.Errorf("poly.pizza asset %s has no thumbnail", a.ID)
	}
	data, code, _, err := httpGet(ctx, p.http, a.PreviewURL, nil)
	if err != nil || code != http.StatusOK {
		return nil, "", "", fmt.Errorf("thumbnail fetch failed (HTTP %d): %v", code, err)
	}
	mime := sniffImage(data)
	path, _ := cachePreviewPath(model.SourcePolyPizza, a.ID, a.PreviewURL+extForMime(mime))
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, data, 0o644)
	}
	return data, mime, path, nil
}

// Download fetches the model file. The /download endpoint either streams a
// file or returns JSON {"url": ...} pointing at the real file.
func (p *PolyPizza) Download(ctx context.Context, a model.Asset, destDir, mode string) (*model.DownloadResult, error) {
	dl := a.DownloadURL
	if dl == "" {
		m, err := p.Model(ctx, strings.TrimPrefix(a.ID, model.SourcePolyPizza+":"))
		if err != nil {
			return nil, err
		}
		dl = m.DownloadURL
	}
	if dl == "" {
		return nil, fmt.Errorf("no download URL for poly.pizza asset %s", a.ID)
	}
	data, code, hdr, err := httpGet(ctx, p.http, dl, p.headers())
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("poly.pizza download", code, data)
	}
	// JSON envelope → follow the embedded URL.
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var env struct {
			URL      string `json:"url"`
			Download string `json:"download"`
		}
		if err := json.Unmarshal(trimmed, &env); err == nil {
			real := env.URL
			if real == "" {
				real = env.Download
			}
			if real != "" {
				data, code, hdr, err = httpGet(ctx, p.http, real, p.headers())
				if err != nil {
					return nil, err
				}
				if code != http.StatusOK {
					return nil, statusErr("poly.pizza download", code, data)
				}
				dl = real
			}
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	fname := filenameFromURL(dl, hdr, "polypizza_"+strings.ReplaceAll(a.ID, ":", "_"))
	dst := filepath.Join(destDir, fname)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return nil, err
	}
	files := []string{dst}
	total := int64(len(data))
	if strings.EqualFold(filepath.Ext(fname), ".zip") {
		extracted, err := unzip(data, destDir)
		if err == nil {
			files = extracted
			total = 0
			for _, f := range files {
				if st, err := os.Stat(f); err == nil {
					total += st.Size()
				}
			}
		}
	}
	return &model.DownloadResult{
		Status:     model.StatusDownloaded,
		Files:      files,
		TotalBytes: total,
		License:    a.License,
		URL:        a.URL,
		Message:    licenseMessage(a),
	}, nil
}

func licenseMessage(a model.Asset) string {
	switch {
	case strings.EqualFold(a.License, "CC0"):
		return "CC0 (public domain): no attribution required."
	case a.License != "":
		return fmt.Sprintf("License %s: check attribution requirements before shipping.", a.License)
	default:
		return "Check the source page for license terms."
	}
}

func filenameFromURL(raw string, hdr http.Header, fallback string) string {
	if cd := hdr.Get("Content-Disposition"); cd != "" {
		if i := strings.Index(cd, "filename="); i >= 0 {
			name := strings.Trim(strings.TrimPrefix(cd[i+len("filename="):], `"`), `"`)
			if name != "" {
				return sanitizeFilename(name)
			}
		}
	}
	u, err := url.Parse(raw)
	if err == nil && u.Path != "" {
		base := filepath.Base(u.Path)
		if base != "" && base != "/" && base != "." {
			return sanitizeFilename(base)
		}
	}
	return sanitizeFilename(fallback + ".zip")
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || name == ".." {
		return "download.bin"
	}
	return name
}

func unzip(data []byte, destDir string) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range zr.File {
		name := sanitizeFilename(f.Name)
		dst := filepath.Join(destDir, name)
		if !strings.HasPrefix(dst, filepath.Clean(destDir)+string(os.PathSeparator)) {
			continue // zip-slip guard
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(dst, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return out, err
		}
		rc, err := f.Open()
		if err != nil {
			return out, err
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, io.LimitReader(rc, 1<<30)); err != nil {
			rc.Close()
			return out, err
		}
		rc.Close()
		if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
			return out, err
		}
		out = append(out, dst)
	}
	return out, nil
}
