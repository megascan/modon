// Package server wires the local index, the remote sources and the MCP
// tool layer together.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"modon/internal/config"
	importdir "modon/internal/importer"
	"modon/internal/model"
	"modon/internal/source"
	"modon/internal/store"
)

// Deps are the shared handles every tool needs.
type Deps struct {
	Cfg *config.Config
	St  *store.Store
	Reg *source.Registry
}

// NewDeps opens the config, the SQLite index and the source registry.
func NewDeps() (*Deps, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	dbPath, err := config.DBPath()
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Deps{Cfg: cfg, St: st, Reg: source.NewRegistry(cfg, st)}, nil
}

func (d *Deps) Close() error {
	if d.St != nil {
		return d.St.Close()
	}
	return nil
}

// NewMCPServer builds the MCP server with all modon tools registered.
func NewMCPServer(d *Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "modon", Version: "0.1.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_assets",
		Description: "Search 3D mesh assets across owned/local index first, then poly.pizza and Fab. Supports free-text, tags, category, style, asset type, animation filter, price range and source selection. Paid results are flagged purchase_required per config (ask/allow/deny) so the agent can ask the user before buying.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, any, error) {
		return handleSearch(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_asset_details",
		Description: "Get full details for one asset by namespaced id (local:<rowid>, polypizza:<id>, fab:<uid>).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
		return handleDetails(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_preview",
		Description: "Get a preview image for an asset. Returns the image inline plus the cached file path. Local assets are rendered with Blender/f3d on demand when no thumbnail exists.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
		return handlePreview(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "download_asset",
		Description: "Download (or reference) an asset. Local assets are copied/symlinked/referenced. Remote free assets are downloaded. Paid or unclaimed Fab assets return purchase_required or claim_required with the listing URL and price — ask the user whether to proceed, never purchase on their behalf. Modes: copy (default), symlink, reference (local only, no files copied).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in DownloadInput) (*mcp.CallToolResult, any, error) {
		return handleDownload(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "import_assets",
		Description: "Import (setup) a directory of mesh files into the local index. Parses glTF/GLB/OBJ/FBX/STL/PLY/DAE, infers category, art-style descriptors and tags, and optionally renders Blender/f3d thumbnails. This is the setup command for onboarding owned asset libraries.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ImportInput) (*mcp.CallToolResult, any, error) {
		return handleImport(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "update_asset_metadata",
		Description: "Enrich a local asset with a rich description, category, art-style descriptors, tags or asset type. Clears needs_review once a description exists. Use after reviewing list_unclassified results.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in UpdateMetadataInput) (*mcp.CallToolResult, any, error) {
		return handleUpdateMetadata(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_unclassified",
		Description: "List local assets that still need review (no rich description yet), largest files first.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in LimitInput) (*mcp.CallToolResult, any, error) {
		return handleUnclassified(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "index_stats",
		Description: "Show local index statistics (totals, needs_review count, breakdowns by type/category/style, animated count, FTS status) plus server config status (which sources/credentials are configured).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in EmptyInput) (*mcp.CallToolResult, any, error) {
		return handleStats(ctx, d, in)
	})

	return s
}

// ---- tool inputs ----

type SearchInput struct {
	Query     string   `json:"query,omitempty" jsonschema:"Free-text search, e.g. low-poly wooden chair"`
	Tags      []string `json:"tags,omitempty" jsonschema:"Extra keywords that should match, e.g. [wooden, chair]"`
	Category  string   `json:"category,omitempty" jsonschema:"Category filter, e.g. chair, vehicle, building, character"`
	Style     string   `json:"style,omitempty" jsonschema:"Art-style filter, e.g. lowpoly, realistic, stylized, voxel"`
	AssetType string   `json:"asset_type,omitempty" jsonschema:"One of model, animation, material, texture, other"`
	Animated  *bool    `json:"animated,omitempty" jsonschema:"When true return only animated assets, when false only static ones"`
	FreeOnly  bool     `json:"free_only,omitempty" jsonschema:"When true return only free assets"`
	MaxPrice  float64  `json:"max_price,omitempty" jsonschema:"Maximum price override for this search, 0 means use server config"`
	Sources   []string `json:"sources,omitempty" jsonschema:"Subset of sources to search: local, polypizza, fab. Default searches all configured sources owned-first."`
	Limit     int      `json:"limit,omitempty" jsonschema:"Max results per search, default 12"`
}

type IDInput struct {
	ID string `json:"id" jsonschema:"Namespaced asset id: local:<rowid>, polypizza:<id> or fab:<uid>"`
}

type DownloadInput struct {
	ID      string `json:"id" jsonschema:"Namespaced asset id: local:<rowid>, polypizza:<id> or fab:<uid>"`
	DestDir string `json:"dest_dir,omitempty" jsonschema:"Destination directory. Defaults to the server download dir."`
	Mode    string `json:"mode,omitempty" jsonschema:"Local assets: copy (default), symlink or reference (no files copied, paths only). Remote assets always download."`
}

type ImportInput struct {
	Directory      string `json:"directory" jsonschema:"Directory of mesh files to import"`
	Recursive      *bool  `json:"recursive,omitempty" jsonschema:"Recurse into subdirectories, default true"`
	RenderPreviews bool   `json:"render_previews,omitempty" jsonschema:"Render Blender/f3d thumbnails for assets without one"`
	MaxPreviews    int    `json:"max_previews,omitempty" jsonschema:"Cap on thumbnails rendered in one run, 0 means unlimited"`
}

type UpdateMetadataInput struct {
	ID          string   `json:"id" jsonschema:"Local asset id: local:<rowid>"`
	Description string   `json:"description,omitempty" jsonschema:"Rich description of the asset"`
	Category    string   `json:"category,omitempty" jsonschema:"Canonical category, e.g. chair, vehicle, character"`
	Style       string   `json:"style,omitempty" jsonschema:"Art-style descriptors, e.g. lowpoly, stylized"`
	AssetType   string   `json:"asset_type,omitempty" jsonschema:"One of model, animation, material, texture, other"`
	Tags        []string `json:"tags,omitempty" jsonschema:"Tags to merge into the asset"`
}

type LimitInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"Max rows to return, default 20"`
}

type EmptyInput struct{}

// ---- handlers ----

func handleSearch(ctx context.Context, d *Deps, in SearchInput) (*mcp.CallToolResult, any, error) {
	q := model.Query{
		Text:      in.Query,
		Tags:      in.Tags,
		Category:  in.Category,
		Style:     in.Style,
		AssetType: in.AssetType,
		Animated:  in.Animated,
		Limit:     in.Limit,
	}
	if in.FreeOnly {
		t := true
		q.Free = &t
	}
	if in.MaxPrice > 0 {
		q.MaxPrice = in.MaxPrice
	}
	out, err := d.Reg.Search(ctx, q, in.Sources)
	if err != nil {
		return errResult(err), nil, nil
	}
	summary := summarizeSearch(out)
	text, _ := json.MarshalIndent(summary, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, out, nil
}

type searchSummary struct {
	Total   int           `json:"total"`
	Assets  []model.Asset `json:"assets"`
	Notes   []string      `json:"notes,omitempty"`
	Errors  []string      `json:"errors,omitempty"`
	Sources []string      `json:"sources_searched"`
	Hint    string        `json:"hint,omitempty"`
}

func summarizeSearch(out *source.Outcome) searchSummary {
	s := searchSummary{
		Total:   len(out.Results),
		Assets:  out.Results,
		Notes:   out.Notes,
		Errors:  out.Errors,
		Sources: out.Searched,
	}
	for _, a := range out.Results {
		if a.PurchaseRequired {
			s.Hint = "Some results require purchase (purchase_required=true). Ask the user whether they are willing to purchase before calling download_asset; download_asset will return purchase_required with price and URL."
			break
		}
	}
	return s
}

func handleDetails(ctx context.Context, d *Deps, in IDInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.ID) == "" {
		return errResult(fmt.Errorf("id is required, e.g. local:3")), nil, nil
	}
	a, err := d.Reg.Get(ctx, in.ID)
	if err != nil {
		return errResult(err), nil, nil
	}
	text, _ := json.MarshalIndent(a, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, a, nil
}

func handlePreview(ctx context.Context, d *Deps, in IDInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.ID) == "" {
		return errResult(fmt.Errorf("id is required")), nil, nil
	}
	data, mime, path, err := d.Reg.Preview(ctx, in.ID)
	if err != nil {
		return errResult(err), nil, nil
	}
	if mime == "" {
		mime = "image/png"
	}
	info := map[string]any{"id": in.ID, "mime": mime, "path": path, "bytes": len(data)}
	infoText, _ := json.MarshalIndent(info, "", "  ")
	out := map[string]any{"id": in.ID, "mime": mime, "path": path}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.ImageContent{Data: data, MIMEType: mime},
			&mcp.TextContent{Text: string(infoText)},
		},
	}, out, nil
}

func handleDownload(ctx context.Context, d *Deps, in DownloadInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.ID) == "" {
		return errResult(fmt.Errorf("id is required")), nil, nil
	}
	mode := in.Mode
	if mode == "" {
		mode = "copy"
	}
	switch mode {
	case "copy", "symlink", "reference":
	default:
		return errResult(fmt.Errorf("mode must be copy, symlink or reference")), nil, nil
	}
	a, res, err := d.Reg.Download(ctx, in.ID, in.DestDir, mode)
	if err != nil {
		return errResult(err), nil, nil
	}
	payload := map[string]any{"asset": a, "result": res}
	text, _ := json.MarshalIndent(payload, "", "  ")
	// Surface the purchase/claim flow explicitly in text so the model acts on it.
	if res.Status == model.StatusPurchaseRequired || res.Status == model.StatusClaimRequired {
		text = append([]byte("ACTION REQUIRED: ask the user before proceeding.\n"), text...)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}

func handleImport(ctx context.Context, d *Deps, in ImportInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Directory) == "" {
		return errResult(fmt.Errorf("directory is required")), nil, nil
	}
	opts := importdir.Options{
		Recursive:      true,
		RenderPreviews: in.RenderPreviews,
		MaxPreviews:    in.MaxPreviews,
	}
	if in.Recursive != nil {
		opts.Recursive = *in.Recursive
	}
	stats, err := importdir.Directory(ctx, d.St, d.Cfg, in.Directory, opts)
	if err != nil {
		return errResult(err), nil, nil
	}
	text, _ := json.MarshalIndent(stats, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, stats, nil
}

func handleUpdateMetadata(_ context.Context, d *Deps, in UpdateMetadataInput) (*mcp.CallToolResult, any, error) {
	src, rid, err := source.SplitID(in.ID)
	if err != nil {
		return errResult(err), nil, nil
	}
	if src != model.SourceLocal {
		return errResult(fmt.Errorf("only local assets can be edited, got %q", in.ID)), nil, nil
	}
	rowID, err := strconv.ParseInt(rid, 10, 64)
	if err != nil {
		return errResult(fmt.Errorf("local asset ids are numeric, got %q", in.ID)), nil, nil
	}
	a, err := d.St.UpdateMetadata(rowID, in.Description, in.Category, in.Style, in.AssetType, in.Tags)
	if err != nil {
		return errResult(err), nil, nil
	}
	text, _ := json.MarshalIndent(a, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, a, nil
}

func handleUnclassified(_ context.Context, d *Deps, in LimitInput) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	assets, err := d.St.ListUnclassified(limit)
	if err != nil {
		return errResult(err), nil, nil
	}
	payload := map[string]any{"count": len(assets), "assets": assets}
	text, _ := json.MarshalIndent(payload, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}

func handleStats(_ context.Context, d *Deps, _ EmptyInput) (*mcp.CallToolResult, any, error) {
	stats, err := d.St.Stats()
	if err != nil {
		return errResult(err), nil, nil
	}
	payload := map[string]any{"index": stats, "config": d.Cfg.Status()}
	text, _ := json.MarshalIndent(payload, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}

func errResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "error: " + err.Error()}},
		IsError: true,
	}
}
