// Package server wires the local index, the remote sources and the MCP
// tool layer together.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"modon/internal/config"
	"modon/internal/embed"
	importdir "modon/internal/importer"
	"modon/internal/model"
	"modon/internal/semantic"
	"modon/internal/source"
	"modon/internal/store"
)

// Deps are the shared handles every tool needs.
type Deps struct {
	Cfg *config.Config
	St  *store.Store
	Reg *source.Registry
	Sem *semantic.Engine
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
	d := &Deps{Cfg: cfg, St: st, Reg: source.NewRegistry(cfg, st), Sem: semantic.New(cfg, st)}
	if d.Sem.Enabled() {
		d.Reg.Local.Searcher = d.Sem
	}
	return d, nil
}

// Warm starts the embedding sidecar when autostart is on and, once it
// answers, embeds whatever changed since the last run (text documents and
// existing previews; rendering stays an explicit `modon embed --render`).
// Search falls back to keyword ranking until then.
func (d *Deps) Warm(ctx context.Context) {
	if !d.Sem.Enabled() {
		return
	}
	go func() {
		if err := d.Sem.Start(ctx, 20*time.Minute); err != nil {
			fmt.Fprintln(os.Stderr, "modon: embedding sidecar:", err)
		}
		stats, err := d.Sem.Backfill(ctx, semantic.BackfillOptions{Text: true, Images: true})
		if errors.Is(err, semantic.ErrBusy) {
			return // another modon (e.g. a second MCP client) is on it
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "modon: embedding backfill:", err)
			return
		}
		if stats.Text+stats.Images > 0 {
			fmt.Fprintf(os.Stderr, "modon: embedded %d text / %d image vectors\n", stats.Text, stats.Images)
		}
	}()
}

func (d *Deps) Close() error {
	d.Sem.Close()
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
		Description: "Search 3D mesh assets across owned/local index first, then poly.pizza and Fab. Supports free-text, tags, category, style, asset type, animation filter, price range and source selection. With semantic search enabled, local results are ranked by meaning and by what the preview thumbnail looks like (EmbeddingGemma 2), so descriptive queries like 'mossy stone ruin' find badly named files; pass image to search the local index by a reference picture, or style_reference (an image or an asset id such as one already used in the scene) to keep results in a consistent art style. Local hits carry score and matched_by (keyword/text/image); copies of one model (FBX + OBJ exports, props shared between packs) are collapsed into the best-ranked one with cluster_id and a duplicates list (set expand_duplicates to get every copy). Paid results are flagged purchase_required per config (ask/allow/deny) so the agent can ask the user before buying.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, any, error) {
		return handleSearch(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "find_similar",
		Description: "Find owned (local) assets that look like or are described like a given asset: local:<rowid> for variants and near-duplicates, or a paid polypizza:/fab: result to check whether the user already owns something equivalent before asking them to buy it. Results are ranked by fused visual+text evidence; similarity is the raw preview-vs-preview cosine (1.0 = identical thumbnail). Needs semantic search (embedding server).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in SimilarInput) (*mcp.CallToolResult, any, error) {
		return handleSimilar(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "suggest_metadata",
		Description: "Zero-shot category and art-style guesses for a local asset from its preview image and text, with scores and a confidence margin. Use while enriching list_unclassified results, then write the result with update_asset_metadata. Needs semantic search.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
		return handleSuggest(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "embed_index",
		Description: "(Re)build semantic vectors for the local index: embeds text documents and preview thumbnails that changed since the last run. render_previews also renders missing thumbnails first (slow: Blender per asset). Use limit to work in chunks. Optionally auto-fills empty categories from confident visual guesses.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in EmbedInput) (*mcp.CallToolResult, any, error) {
		return handleEmbed(ctx, d, in)
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
		Description: "Write metadata for local assets: description, category, art-style descriptors, tags, asset type. Fields you write are recorded as agent-written (provenance) and survive re-imports; heuristic values never overwrite them. Tags merge by default; tags_mode=replace makes tags the complete list, remove_tags deletes tags. Pass ids to tag several assets at once and apply_to_duplicates to copy the edit to every copy in the asset's duplicate cluster. needs_review clears once a description exists, or a category plus tags; this also releases any list_unclassified claim on the asset.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in UpdateMetadataInput) (*mcp.CallToolResult, any, error) {
		return handleUpdateMetadata(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_unclassified",
		Description: "List local assets that still need review, largest files first. Page with offset. For parallel tagging, pass worker (any id unique to you): the returned assets are claimed for you for lease_seconds (default 900) and other workers get disjoint batches; claims are released as you update each asset, and release=true drops your remaining claims. one_per_cluster returns one representative per duplicate cluster (its duplicates are listed and claimed too) so you can tag it once with apply_to_duplicates.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in UnclassifiedInput) (*mcp.CallToolResult, any, error) {
		return handleUnclassified(ctx, d, in)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "index_stats",
		Description: "Show local index statistics (totals, needs_review count, breakdowns by type/category/style, animated count, duplicate clusters, agent-edited assets, active review claims, FTS status), the semantic layer (embedding server reachability checked live, device, vectors) and config status (which sources/credentials are configured).",
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
	Image     string   `json:"image,omitempty" jsonschema:"Query by image: local file path, http(s) URL or data: URL of a reference picture (searches the local index; needs semantic search)"`

	StyleReference string  `json:"style_reference,omitempty" jsonschema:"Keep a consistent look: owned results are re-ranked toward this reference while the query text still decides what the object is. An image path/URL, local:<id> of an owned asset (e.g. one already in the scene), or a polypizza:/fab: id (its preview is used). Needs semantic search."`
	StyleWeight    float64 `json:"style_weight,omitempty" jsonschema:"How much the reference look counts versus relevance, 0..1 (default 0.5; higher = stricter style match)"`

	ExpandDuplicates bool `json:"expand_duplicates,omitempty" jsonschema:"Return every copy of a model (FBX and OBJ exports, the same prop in several packs) instead of collapsing each duplicate cluster into its best-ranked member"`
}

type SimilarInput struct {
	ID        string `json:"id" jsonschema:"Asset to match: local:<rowid>, polypizza:<id> or fab:<uid>"`
	Category  string `json:"category,omitempty" jsonschema:"Restrict matches to this category"`
	Style     string `json:"style,omitempty" jsonschema:"Restrict matches to this art style"`
	AssetType string `json:"asset_type,omitempty" jsonschema:"Restrict matches to model, animation, ..."`
	Animated  *bool  `json:"animated,omitempty" jsonschema:"Restrict to animated (true) or static (false) assets"`
	Limit     int    `json:"limit,omitempty" jsonschema:"Max results, default 12"`
}

type EmbedInput struct {
	SkipText       bool    `json:"skip_text,omitempty" jsonschema:"Do not embed text documents"`
	SkipImages     bool    `json:"skip_images,omitempty" jsonschema:"Do not embed preview images"`
	RenderPreviews bool    `json:"render_previews,omitempty" jsonschema:"Render missing thumbnails before embedding images (slow)"`
	MaxRender      int     `json:"max_render,omitempty" jsonschema:"Cap on thumbnails rendered this run, 0 means unlimited"`
	Limit          int     `json:"limit,omitempty" jsonschema:"Cap on assets embedded per kind this run, 0 means all pending"`
	AutoClassify   bool    `json:"auto_classify,omitempty" jsonschema:"Afterwards fill empty categories/styles with confident zero-shot guesses"`
	MinMargin      float64 `json:"min_margin,omitempty" jsonschema:"Confidence margin for auto_classify, default calibrated value"`
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
	ID                string   `json:"id,omitempty" jsonschema:"Local asset id: local:<rowid>"`
	IDs               []string `json:"ids,omitempty" jsonschema:"Several local asset ids that get the same edit (instead of id)"`
	Description       string   `json:"description,omitempty" jsonschema:"Rich description of the asset"`
	Category          string   `json:"category,omitempty" jsonschema:"Canonical category, e.g. chair, vehicle, character"`
	Style             string   `json:"style,omitempty" jsonschema:"Art-style descriptors, e.g. lowpoly, stylized"`
	AssetType         string   `json:"asset_type,omitempty" jsonschema:"One of model, animation, material, texture, other"`
	Tags              []string `json:"tags,omitempty" jsonschema:"Tags to add (tags_mode merge, default) or the complete tag list (tags_mode replace)"`
	TagsMode          string   `json:"tags_mode,omitempty" jsonschema:"merge (default): add tags to the current list; replace: tags becomes the whole list (an empty list clears it)"`
	RemoveTags        []string `json:"remove_tags,omitempty" jsonschema:"Tags to delete (case-insensitive), applied after merge/replace"`
	ApplyToDuplicates bool     `json:"apply_to_duplicates,omitempty" jsonschema:"Also apply the edit to every other copy in each asset's duplicate cluster"`
	Reviewed          *bool    `json:"reviewed,omitempty" jsonschema:"Force needs_review off (true) or on (false); by default it clears once a description, or a category plus tags, exists"`
}

type UnclassifiedInput struct {
	Limit         int    `json:"limit,omitempty" jsonschema:"Max rows to return, default 20"`
	Offset        int    `json:"offset,omitempty" jsonschema:"Skip this many rows (paging without claims)"`
	Worker        string `json:"worker,omitempty" jsonschema:"Your worker id: claim the returned assets so parallel agents get disjoint batches"`
	LeaseSeconds  int    `json:"lease_seconds,omitempty" jsonschema:"How long claims last, default 900"`
	OnePerCluster bool   `json:"one_per_cluster,omitempty" jsonschema:"One representative per duplicate cluster; its duplicates are listed (and claimed with worker)"`
	Release       bool   `json:"release,omitempty" jsonschema:"With worker: release your remaining claims instead of listing"`
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
		Image:     in.Image,

		ExpandDuplicates: in.ExpandDuplicates,

		StyleWeight: in.StyleWeight,
	}
	if in.StyleReference != "" {
		ref, err := StyleRef(ctx, d, in.StyleReference)
		if err != nil {
			return errResult(err), nil, nil
		}
		q.StyleRef = ref
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
			s.Hint = "Some results require purchase (purchase_required=true). Ask the user whether they are willing to purchase before calling download_asset; download_asset will return purchase_required with price and URL. find_similar on a paid result shows owned look-alikes first."
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
	// A freshly rendered local thumbnail becomes searchable right away.
	if src, rid, err := source.SplitID(in.ID); err == nil && src == model.SourceLocal && d.Sem.Enabled() {
		if rowID, err := strconv.ParseInt(rid, 10, 64); err == nil {
			go d.Sem.Refresh(context.WithoutCancel(ctx), rowID)
		}
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
	payload := map[string]any{"import": stats}
	if !d.Sem.Enabled() {
		if ds, err := semantic.Dedupe(d.St, nil); err == nil {
			payload["duplicates"] = ds
		}
	}
	if d.Sem.Enabled() {
		if es, err := d.Sem.Backfill(ctx, semantic.BackfillOptions{Text: true, Images: true}); err != nil {
			payload["embedding_error"] = err.Error()
		} else {
			payload["embedding"] = es
		}
	}
	text, _ := json.MarshalIndent(payload, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}

func handleUpdateMetadata(ctx context.Context, d *Deps, in UpdateMetadataInput) (*mcp.CallToolResult, any, error) {
	refs := in.IDs
	if in.ID != "" {
		refs = append([]string{in.ID}, refs...)
	}
	if len(refs) == 0 {
		return errResult(fmt.Errorf("id (or ids) is required, e.g. local:3")), nil, nil
	}
	u := store.MetadataUpdate{
		Description: in.Description, Category: in.Category, Style: in.Style, AssetType: in.AssetType,
		Tags: in.Tags, TagsMode: in.TagsMode, RemoveTags: in.RemoveTags, Reviewed: in.Reviewed,
	}
	updated, err := UpdateMetadata(ctx, d, refs, u, in.ApplyToDuplicates)
	if err != nil {
		return errResult(err), nil, nil
	}
	var payload any = map[string]any{"updated": len(updated), "assets": updated}
	if len(updated) == 1 {
		payload = updated[0] // single edits keep the original response shape
	}
	text, _ := json.MarshalIndent(payload, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}

// UpdateMetadata applies one agent edit to local assets (and, with
// duplicates, to every copy in their clusters), then re-embeds them.
func UpdateMetadata(ctx context.Context, d *Deps, refs []string, u store.MetadataUpdate, duplicates bool) ([]model.Asset, error) {
	if u.Empty() {
		return nil, fmt.Errorf("nothing to update: pass description, category, style, asset_type, tags, remove_tags or reviewed")
	}
	var ids []int64
	seen := map[int64]bool{}
	add := func(id int64) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, ref := range refs {
		id, err := LocalID(ref)
		if err != nil {
			return nil, err
		}
		add(id)
		if duplicates {
			a, err := d.St.GetByID(id)
			if err != nil {
				return nil, err
			}
			members, err := d.St.ClusterMembers(a.ClusterID)
			if err != nil {
				return nil, err
			}
			for _, m := range members {
				add(m)
			}
		}
	}
	out := make([]model.Asset, 0, len(ids))
	for _, id := range ids {
		a, err := d.St.UpdateMetadata(id, u)
		if err != nil {
			return out, fmt.Errorf("local:%d: %w", id, err)
		}
		out = append(out, a)
	}
	d.Sem.Refresh(ctx, ids...)
	return out, nil
}

// LocalID parses "local:<rowid>".
func LocalID(ref string) (int64, error) {
	src, rid, err := source.SplitID(ref)
	if err != nil {
		return 0, err
	}
	if src != model.SourceLocal {
		return 0, fmt.Errorf("only local assets can be edited, got %q", ref)
	}
	id, err := strconv.ParseInt(rid, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("local asset ids are numeric, got %q", ref)
	}
	return id, nil
}

func handleUnclassified(_ context.Context, d *Deps, in UnclassifiedInput) (*mcp.CallToolResult, any, error) {
	var payload any
	if in.Release {
		n, err := d.St.ReleaseClaims(in.Worker)
		if err != nil {
			return errResult(err), nil, nil
		}
		payload = map[string]any{"worker": in.Worker, "released": n}
	} else {
		page, err := d.St.ListUnclassified(store.UnclassifiedOptions{
			Limit: in.Limit, Offset: in.Offset, Worker: in.Worker,
			Lease: time.Duration(in.LeaseSeconds) * time.Second, OnePerCluster: in.OnePerCluster,
		})
		if err != nil {
			return errResult(err), nil, nil
		}
		m := map[string]any{"count": len(page.Assets), "assets": page.Assets, "remaining": page.Remaining}
		if page.Worker != "" {
			m["worker"], m["lease_expires_at"] = page.Worker, page.LeaseExpiresAt
		}
		payload = m
	}
	text, _ := json.MarshalIndent(payload, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}

func handleStats(ctx context.Context, d *Deps, _ EmptyInput) (*mcp.CallToolResult, any, error) {
	stats, err := d.St.Stats()
	if err != nil {
		return errResult(err), nil, nil
	}
	payload := map[string]any{"index": stats, "config": d.Cfg.Status(), "semantic": d.Sem.Status(ctx)}
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

// StyleRef turns a style reference into something the semantic engine can
// embed: local ids and images pass through; remote asset ids become their
// preview image.
func StyleRef(ctx context.Context, d *Deps, ref string) (string, error) {
	src, _, err := source.SplitID(ref)
	if err != nil || src == model.SourceLocal || strings.Contains(ref, "/") {
		return ref, nil // a path/URL, or local:<id>
	}
	if src != model.SourcePolyPizza && src != model.SourceFab {
		return ref, nil
	}
	data, mime, _, err := d.Reg.Preview(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("style reference %s: %w", ref, err)
	}
	return embed.DataURL(data, mime), nil
}

// SimilarRef resolves an asset id into a semantic reference, fetching remote
// previews so they can be compared against owned thumbnails.
func SimilarRef(ctx context.Context, d *Deps, id string) (semantic.Ref, error) {
	a, err := d.Reg.Get(ctx, id)
	if err != nil {
		return semantic.Ref{}, err
	}
	ref := semantic.Ref{Asset: *a}
	if a.Source != model.SourceLocal {
		if data, mime, _, err := d.Reg.Preview(ctx, id); err == nil {
			ref.Image, ref.ImageMIME = data, mime
		}
	}
	return ref, nil
}

func handleSimilar(ctx context.Context, d *Deps, in SimilarInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.ID) == "" {
		return errResult(fmt.Errorf("id is required")), nil, nil
	}
	ref, err := SimilarRef(ctx, d, in.ID)
	if err != nil {
		return errResult(err), nil, nil
	}
	assets, err := d.Sem.Similar(ctx, ref, model.Query{
		Category: in.Category, Style: in.Style, AssetType: in.AssetType,
		Animated: in.Animated, Limit: in.Limit,
	})
	if err != nil {
		return errResult(err), nil, nil
	}
	payload := map[string]any{"reference": ref.Asset.ID, "count": len(assets), "owned_matches": assets}
	if ref.Asset.PurchaseRequired || (!ref.Asset.IsFree && ref.Asset.Price > 0) {
		payload["hint"] = "The reference is paid. If an owned match fits, offer it to the user before suggesting a purchase."
	}
	text, _ := json.MarshalIndent(payload, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}

func handleSuggest(ctx context.Context, d *Deps, in IDInput) (*mcp.CallToolResult, any, error) {
	src, rid, err := source.SplitID(in.ID)
	if err != nil {
		return errResult(err), nil, nil
	}
	if src != model.SourceLocal {
		return errResult(fmt.Errorf("suggestions are for local assets, got %q", in.ID)), nil, nil
	}
	rowID, err := strconv.ParseInt(rid, 10, 64)
	if err != nil {
		return errResult(fmt.Errorf("local asset ids are numeric, got %q", in.ID)), nil, nil
	}
	sg, err := d.Sem.Suggest(ctx, rowID)
	if err != nil {
		return errResult(err), nil, nil
	}
	text, _ := json.MarshalIndent(sg, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, sg, nil
}

func handleEmbed(ctx context.Context, d *Deps, in EmbedInput) (*mcp.CallToolResult, any, error) {
	if !d.Sem.Enabled() {
		return errResult(fmt.Errorf("semantic search is off: set embedding.endpoint (modon config set embedding.endpoint http://127.0.0.1:8178) and run modon embed-server, or set embedding.autostart true")), nil, nil
	}
	stats, err := d.Sem.Backfill(ctx, semantic.BackfillOptions{
		Text: !in.SkipText, Images: !in.SkipImages,
		Render: in.RenderPreviews, MaxRender: in.MaxRender, Limit: in.Limit,
	})
	if err != nil {
		return errResult(err), nil, nil
	}
	payload := map[string]any{"embedding": stats}
	if in.AutoClassify {
		cs, err := d.Sem.AutoClassify(ctx, true, in.MinMargin)
		if err != nil {
			payload["classify_error"] = err.Error()
		} else {
			payload["classify"] = cs
		}
	}
	text, _ := json.MarshalIndent(payload, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(text)}},
	}, payload, nil
}
