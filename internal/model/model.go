// Package model defines the shared asset and query types used by the local
// index, the remote sources, and the MCP tool layer.
package model

// Source names.
const (
	SourceLocal     = "local"
	SourcePolyPizza = "polypizza"
	SourceFab       = "fab"
)

// Asset is the unified view of a 3D asset, whether it lives on disk (local
// index) or on a remote marketplace (poly.pizza, Fab).
type Asset struct {
	ID          string `json:"id"`     // namespaced, e.g. "local:42", "polypizza:abc", "fab:<uid>"
	Source      string `json:"source"` // local | polypizza | fab
	Title       string `json:"title"`
	Name        string `json:"name,omitempty"` // file/base name for local assets
	Description string `json:"description,omitempty"`

	Creator string `json:"creator,omitempty"`
	License string `json:"license,omitempty"`

	AssetType string   `json:"asset_type"` // model | animation | material | texture | other
	Category  string   `json:"category,omitempty"`
	Style     string   `json:"style,omitempty"`
	Tags      []string `json:"tags,omitempty"`

	Animated       bool     `json:"animated"`
	AnimationNames []string `json:"animation_names,omitempty"`

	TriCount  int64    `json:"tri_count,omitempty"`
	VertCount int64    `json:"vert_count,omitempty"`
	Formats   []string `json:"formats,omitempty"` // file extensions without dot: glb, fbx, ...

	// Local-index specific.
	PrimaryFile   string   `json:"primary_file,omitempty"` // absolute path
	Files         []string `json:"files,omitempty"`        // all files belonging to the asset
	FileSize      int64    `json:"file_size,omitempty"`
	TextureCount  int      `json:"texture_count,omitempty"`
	MaterialNames []string `json:"material_names,omitempty"`
	NeedsReview   bool     `json:"needs_review"` // true until a rich description exists

	// Remote-source specific.
	PreviewURL  string `json:"preview_url,omitempty"`
	PreviewPath string `json:"preview_path,omitempty"` // cached local image path
	URL         string `json:"url,omitempty"`          // canonical page/listing URL
	DownloadURL string `json:"download_url,omitempty"` // direct download endpoint, if known

	Price            float64 `json:"price,omitempty"`
	Currency         string  `json:"currency,omitempty"`
	IsFree           bool    `json:"is_free"`
	Owned            bool    `json:"owned"`
	PurchaseRequired bool    `json:"purchase_required,omitempty"`

	Rating      float64 `json:"rating,omitempty"`
	ReviewCount int     `json:"review_count,omitempty"`

	// Search diagnostics: fused relevance (higher is better) and which
	// signals matched (keyword, text, image).
	Score     float64  `json:"score,omitempty"`
	MatchedBy []string `json:"matched_by,omitempty"`
	// Similarity is the raw preview-vs-preview cosine in find_similar
	// results (1.0 = identical thumbnail).
	Similarity float64 `json:"similarity,omitempty"`

	RowID int64 `json:"-"` // local index row id
}

// Query is a normalized search request. Fields with no value are ignored.
type Query struct {
	Text  string // free text
	Image string // query-by-image: local path, http(s) URL or data: URL (local index only)
	// StyleRef re-ranks results toward a look: an image (path, URL, data:
	// URL) or "local:<id>" of an owned asset. StyleWeight 0..1 sets how much
	// the look counts against relevance (0 = default 0.5).
	StyleRef    string
	StyleWeight float64
	Tags        []string // extra keywords that must/may match
	Category    string
	Style       string
	AssetType   string
	Animated    *bool
	Free        *bool
	MinPrice    float64
	MaxPrice    float64 // 0 = no limit
	Limit       int
}

// DownloadResult describes the outcome of a download call. Status is one of
// "downloaded", "reference", "purchase_required", "claim_required",
// "unavailable" or "declined".
type DownloadResult struct {
	Status     string   `json:"status"`
	Files      []string `json:"files,omitempty"`
	TotalBytes int64    `json:"total_bytes,omitempty"`
	License    string   `json:"license,omitempty"`
	URL        string   `json:"url,omitempty"`   // purchase / listing URL
	Price      float64  `json:"price,omitempty"` // >0 for purchase_required
	Currency   string   `json:"currency,omitempty"`
	Message    string   `json:"message,omitempty"`
}

// Purchase statuses.
const (
	StatusDownloaded       = "downloaded"
	StatusReference        = "reference"
	StatusPurchaseRequired = "purchase_required"
	StatusClaimRequired    = "claim_required"
	StatusDeclined         = "declined"
	StatusUnavailable      = "unavailable"
)
