// Command modon is a Go MCP server (plus CLI) that agents use to find,
// preview and download 3D mesh assets from three sources:
//
//  1. a local SQLite index of owned models (built by `modon setup` / `import`)
//  2. poly.pizza (via API key)
//  3. Fab.com (catalog search; owned downloads need a session cookie)
//
// Owned assets are searched first; paid results are gated by the purchase
// policy (ask/allow/deny) and flagged purchase_required so the agent can ask
// the user before buying — modon never purchases on the user's behalf.
//
// Run with no arguments (or `serve`) to start the MCP server over stdio.
// Run `modon help` for the CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"modon/internal/config"
	"modon/internal/embed"
	importdir "modon/internal/importer"
	"modon/internal/model"
	"modon/internal/semantic"
	"modon/internal/server"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		serve()
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		serve()
		return
	case "setup":
		err = cmdSetup(args)
	case "import":
		err = cmdImport(args)
	case "search":
		err = cmdSearch(args)
	case "details":
		err = cmdDetails(args)
	case "preview":
		err = cmdPreview(args)
	case "download":
		err = cmdDownload(args)
	case "embed":
		err = cmdEmbed(args)
	case "embed-server":
		err = cmdEmbedServer(args)
	case "similar":
		err = cmdSimilar(args)
	case "suggest":
		err = cmdSuggest(args)
	case "config":
		err = cmdConfig(args)
	case "stats":
		err = cmdStats(args)
	case "help", "-h", "--help":
		usage()
		return
	case "version", "-v", "--version":
		fmt.Println("modon", version)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`modon - MCP server for 3D mesh assets (local index + poly.pizza + Fab)

Usage:
  modon [serve]                 Run the MCP server over stdio (default)
  modon setup [flags] <dir>     First-time setup: write config, import meshes, classify
  modon import [flags] <dir>    Import a directory of meshes into the local index
  modon search [flags] <query>  Search owned first, then poly.pizza + Fab
  modon details <id>            Show full details (local:N, polypizza:ID, fab:UID)
  modon preview [flags] <id>    Fetch/render a preview image
  modon download [flags] <id>   Download (or reference) an asset
  modon embed [flags]           Build/refresh semantic vectors (text + preview images)
  modon embed-server [flags]    Run the EmbeddingGemma 2 sidecar (needs uv)
  modon similar [flags] <id>    Owned assets that look like <id> (local or remote)
  modon suggest <id>            Zero-shot category/style guesses for a local asset
  modon config [get|set|path]   Inspect or change configuration
  modon stats                   Local index statistics + config status
  modon help | version

NOTE: flags must come before positional args, e.g.
  modon preview --out thumb.png local:3   (not: modon preview local:3 --out ...)

Setup flags:
  --polypizza-key KEY   poly.pizza API key (or MODON_POLYPIZZA_API_KEY)
  --fab-cookie COOKIE   fab.com session cookie (or MODON_FAB_COOKIE)
  --purchase-policy P   ask (default) | allow | deny
  --max-price F         exclude results above this price (0 = unlimited)
  --no-recursive        do not recurse into subdirectories
  --previews            render Blender/f3d thumbnails during import
  --max-previews N      cap thumbnails rendered (0 = unlimited)

Search flags:
  --category C --style S --type T (model|animation|material|texture|other)
  --animated / --static / --free / --max-price F
  --sources local,polypizza,fab   --limit N (default 12)   --json
  --image PATH|URL                query the local index by a reference picture

Semantic search (EmbeddingGemma 2):
  modon config set embedding.endpoint http://127.0.0.1:8178
  modon config set embedding.autostart true   # serve/embed launch the sidecar
  modon embed --render                        # render missing previews + embed all
Embed flags:
  --no-text --no-images --render --max-render N --limit N
  --classify (fill empty categories; dry run unless --apply) --margin F
  --eval (zero-shot accuracy vs. importer categories)
Embed-server flags:
  --port N --device cuda|cpu|mps --image-tokens N --text-only

Examples:
  modon setup --previews ~/Assets/Models
  modon search "low-poly wooden chair" --free --limit 8
  modon details local:3
  modon download polypizza:abc123 --dest ./assets
`)
}

func serve() {
	d, err := server.NewDeps()
	if err != nil {
		fmt.Fprintln(os.Stderr, "modon:", err)
		os.Exit(1)
	}
	defer d.Close()
	ctx := context.Background()
	d.Warm(ctx)
	s := server.NewMCPServer(d)
	if err := s.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "modon:", err)
		os.Exit(1)
	}
}

func openDeps() (*server.Deps, error) { return server.NewDeps() }

// ---- setup / import ----

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	ppKey := fs.String("polypizza-key", "", "poly.pizza API key")
	fabCookie := fs.String("fab-cookie", "", "fab.com session cookie")
	policy := fs.String("purchase-policy", "", "ask|allow|deny")
	maxPrice := fs.Float64("max-price", -1, "max price (0 = unlimited)")
	noRec := fs.Bool("no-recursive", false, "do not recurse")
	previews := fs.Bool("previews", false, "render thumbnails")
	maxPrev := fs.Int("max-previews", 0, "cap thumbnails")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: modon setup [flags] <directory>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *ppKey != "" {
		cfg.PolyPizzaAPIKey = *ppKey
	}
	if *fabCookie != "" {
		cfg.FabCookie = *fabCookie
	}
	if *policy != "" {
		cfg.PurchasePolicy = *policy
	}
	if *maxPrice >= 0 {
		cfg.MaxPrice = *maxPrice
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	path, _ := config.ConfigPath()
	fmt.Println("wrote config:", path)

	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	stats, err := importdir.Directory(context.Background(), d.St, d.Cfg, fs.Arg(0), importdir.Options{
		Recursive:      !*noRec,
		RenderPreviews: *previews,
		MaxPreviews:    *maxPrev,
		Progress: func(done, total int, path string) {
			if done == 1 || done == total || done%50 == 0 {
				fmt.Fprintf(os.Stderr, "import %d/%d %s\n", done, total, filepath.Base(path))
			}
		},
	})
	if err != nil {
		return err
	}
	printJSON(stats)
	fmt.Fprintf(os.Stderr, "done: %d added, %d updated, %d skipped, %d previews\n",
		stats.Added, stats.Updated, stats.Skipped, stats.Previews)
	if len(stats.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "first errors:\n")
		for i, e := range stats.Errors {
			if i >= 5 {
				break
			}
			fmt.Fprintf(os.Stderr, "  - %s\n", e)
		}
	}
	return nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	noRec := fs.Bool("no-recursive", false, "do not recurse")
	previews := fs.Bool("previews", false, "render thumbnails")
	maxPrev := fs.Int("max-previews", 0, "cap thumbnails")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: modon import [--previews] <directory>")
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	stats, err := importdir.Directory(context.Background(), d.St, d.Cfg, fs.Arg(0), importdir.Options{
		Recursive:      !*noRec,
		RenderPreviews: *previews,
		MaxPreviews:    *maxPrev,
		Progress: func(done, total int, path string) {
			if done == 1 || done == total || done%50 == 0 {
				fmt.Fprintf(os.Stderr, "import %d/%d %s\n", done, total, filepath.Base(path))
			}
		},
	})
	if err != nil {
		return err
	}
	printJSON(stats)
	return nil
}

// ---- search / details / preview / download ----

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	category := fs.String("category", "", "")
	style := fs.String("style", "", "")
	atype := fs.String("type", "", "")
	animated := fs.Bool("animated", false, "")
	staticOnly := fs.Bool("static", false, "")
	free := fs.Bool("free", false, "")
	maxPrice := fs.Float64("max-price", 0, "")
	sources := fs.String("sources", "", "")
	limit := fs.Int("limit", 12, "")
	asJSON := fs.Bool("json", false, "")
	image := fs.String("image", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := model.Query{
		Text:      strings.Join(fs.Args(), " "),
		Image:     *image,
		Category:  *category,
		Style:     *style,
		AssetType: *atype,
		Limit:     *limit,
	}
	if *animated {
		t := true
		q.Animated = &t
	} else if *staticOnly {
		f := false
		q.Animated = &f
	}
	if *free {
		t := true
		q.Free = &t
	}
	if *maxPrice > 0 {
		q.MaxPrice = *maxPrice
	}
	var srcs []string
	if *sources != "" {
		for _, s := range strings.Split(*sources, ",") {
			if s = strings.TrimSpace(s); s != "" {
				srcs = append(srcs, s)
			}
		}
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	startSidecar(d, 0)
	out, err := d.Reg.Search(context.Background(), q, srcs)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("%d result(s) [searched: %s]\n", len(out.Results), strings.Join(out.Searched, ", "))
	for _, n := range out.Notes {
		fmt.Println("note:", n)
	}
	for _, e := range out.Errors {
		fmt.Println("error:", e)
	}
	for _, a := range out.Results {
		price := "free"
		if !a.IsFree && a.Price > 0 {
			price = fmt.Sprintf("%s %.2f", a.Currency, a.Price)
		}
		flag := ""
		if a.PurchaseRequired {
			flag = " [purchase_required]"
		} else if a.Owned {
			flag = " [owned]"
		}
		anim := ""
		if a.Animated {
			anim = " animated"
		}
		match := ""
		if len(a.MatchedBy) > 0 {
			match = fmt.Sprintf(" | %.3f %s", a.Score, strings.Join(a.MatchedBy, "+"))
		}
		fmt.Printf("- %s | %s | %s | %s%s%s%s\n", a.ID, a.Title, price, a.Source, anim, flag, match)
		if a.Description != "" {
			desc := a.Description
			if len(desc) > 160 {
				desc = desc[:157] + "..."
			}
			fmt.Printf("    %s\n", desc)
		}
	}
	for _, a := range out.Results {
		if a.PurchaseRequired {
			fmt.Println("\nSome results require purchase. Ask the user before downloading; modon never buys on their behalf.")
			break
		}
	}
	return nil
}

func cmdDetails(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: modon details <id>")
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	a, err := d.Reg.Get(context.Background(), args[0])
	if err != nil {
		return err
	}
	printJSON(a)
	return nil
}

func cmdPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	out := fs.String("out", "", "write image to this path (default: print cached path)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: modon preview <id> [--out file]")
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	data, mime, path, err := d.Reg.Preview(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%s, %d bytes, cached at %s)\n", *out, mime, len(data), path)
		return nil
	}
	fmt.Printf("preview: %s (%s, %d bytes)\n", path, mime, len(data))
	return nil
}

func cmdDownload(args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	dest := fs.String("dest", "", "")
	mode := fs.String("mode", "copy", "copy|symlink|reference")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: modon download <id> [--dest dir] [--mode copy|symlink|reference]")
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	a, res, err := d.Reg.Download(context.Background(), fs.Arg(0), *dest, *mode)
	if err != nil {
		return err
	}
	printJSON(map[string]any{"asset": a, "result": res})
	switch res.Status {
	case model.StatusPurchaseRequired, model.StatusClaimRequired:
		fmt.Fprintln(os.Stderr, "ACTION REQUIRED:", res.Message)
	}
	return nil
}

// ---- config / stats ----

func cmdConfig(args []string) error {
	if len(args) == 0 {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		masked := map[string]any{
			"polypizza_api_key_set": cfg.PolyPizzaAPIKey != "",
			"fab_cookie_set":        cfg.FabCookie != "",
			"purchase_policy":       cfg.PurchasePolicy,
			"max_price":             cfg.MaxPrice,
			"currency":              cfg.Currency,
			"default_sources":       cfg.DefaultSources,
			"preview":               cfg.Preview,
			"fab_auto_claim_free":   cfg.FabAutoClaimFree,
			"http_timeout_seconds":  cfg.HTTPTimeoutSeconds,
			"embedding":             cfg.Embedding,
		}
		printJSON(masked)
		if p, err := config.ConfigPath(); err == nil {
			fmt.Fprintln(os.Stderr, "config:", p)
		}
		return nil
	}
	switch args[0] {
	case "path":
		p, err := config.ConfigPath()
		if err != nil {
			return err
		}
		fmt.Println(p)
		return nil
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: modon config get <key>")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Println(configValue(cfg, args[1]))
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: modon config set <key> <value>")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err := setConfigValue(cfg, args[1], strings.Join(args[2:], " ")); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Println("saved")
		return nil
	default:
		return fmt.Errorf("usage: modon config [get|set|path]")
	}
}

func configValue(cfg *config.Config, key string) string {
	switch strings.ToLower(key) {
	case "polypizza_api_key":
		if cfg.PolyPizzaAPIKey == "" {
			return "(unset)"
		}
		return "(set)"
	case "fab_cookie":
		if cfg.FabCookie == "" {
			return "(unset)"
		}
		return "(set)"
	case "purchase_policy":
		return cfg.PurchasePolicy
	case "max_price":
		return strconv.FormatFloat(cfg.MaxPrice, 'f', -1, 64)
	case "currency":
		return cfg.Currency
	case "default_sources":
		return strings.Join(cfg.DefaultSources, ",")
	case "preview.renderer":
		return cfg.Preview.Renderer
	case "preview.size":
		return strconv.Itoa(cfg.Preview.Size)
	case "fab_auto_claim_free":
		return strconv.FormatBool(cfg.FabAutoClaimFree)
	case "http_timeout_seconds":
		return strconv.Itoa(cfg.HTTPTimeoutSeconds)
	case "preview.workers":
		return strconv.Itoa(cfg.Preview.Workers)
	case "embedding.endpoint":
		return cfg.Embedding.Endpoint
	case "embedding.model":
		return cfg.Embedding.Model
	case "embedding.dims":
		return strconv.Itoa(cfg.Embedding.Dims)
	case "embedding.autostart":
		return strconv.FormatBool(cfg.Embedding.Autostart)
	case "embedding.image_tokens":
		return strconv.Itoa(cfg.Embedding.ImageTokens)
	case "embedding.device":
		return cfg.Embedding.Device
	case "embedding.uv_path":
		return cfg.Embedding.UVPath
	case "embedding.timeout_seconds":
		return strconv.Itoa(cfg.Embedding.TimeoutSeconds)
	default:
		return "unknown key (see modon config with no args)"
	}
}

func setConfigValue(cfg *config.Config, key, val string) error {
	switch strings.ToLower(key) {
	case "polypizza_api_key":
		cfg.PolyPizzaAPIKey = val
	case "fab_cookie":
		cfg.FabCookie = val
	case "purchase_policy":
		cfg.PurchasePolicy = val
	case "max_price":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return err
		}
		cfg.MaxPrice = f
	case "currency":
		cfg.Currency = strings.ToUpper(val)
	case "default_sources":
		var ss []string
		for _, s := range strings.Split(val, ",") {
			if s = strings.TrimSpace(strings.ToLower(s)); s != "" {
				ss = append(ss, s)
			}
		}
		cfg.DefaultSources = ss
	case "preview.renderer":
		cfg.Preview.Renderer = val
	case "preview.size":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		cfg.Preview.Size = n
	case "fab_auto_claim_free":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		cfg.FabAutoClaimFree = b
	case "http_timeout_seconds":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		cfg.HTTPTimeoutSeconds = n
	case "preview.workers":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		cfg.Preview.Workers = n
	case "embedding.endpoint":
		cfg.Embedding.Endpoint = strings.TrimRight(val, "/")
	case "embedding.model":
		cfg.Embedding.Model = val
	case "embedding.dims":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		switch n {
		case 768, 512, 256, 128:
		default:
			return fmt.Errorf("embedding.dims must be 768, 512, 256 or 128")
		}
		cfg.Embedding.Dims = n
	case "embedding.autostart":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		cfg.Embedding.Autostart = b
	case "embedding.image_tokens":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		cfg.Embedding.ImageTokens = n
	case "embedding.device":
		cfg.Embedding.Device = val
	case "embedding.uv_path":
		cfg.Embedding.UVPath = val
	case "embedding.timeout_seconds":
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		cfg.Embedding.TimeoutSeconds = n
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

func cmdStats(_ []string) error {
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	stats, err := d.St.Stats()
	if err != nil {
		return err
	}
	printJSON(map[string]any{"index": stats, "config": d.Cfg.Status(), "semantic": d.Sem.Status(context.Background())})
	return nil
}

// ---- semantic ----

// startSidecar launches the embedding sidecar for CLI commands when
// embedding.autostart is set, waiting up to wait for it to load.
func startSidecar(d *server.Deps, wait time.Duration) {
	if !d.Sem.Enabled() || !d.Cfg.Embedding.Autostart || d.Sem.Reachable(context.Background()) {
		return
	}
	if wait == 0 {
		wait = 3 * time.Minute
	}
	fmt.Fprintln(os.Stderr, "starting embedding sidecar (first run installs torch + downloads the model)...")
	if err := d.Sem.Start(context.Background(), wait); err != nil {
		fmt.Fprintln(os.Stderr, "embedding sidecar:", err)
	}
}

func cmdEmbed(args []string) error {
	fs := flag.NewFlagSet("embed", flag.ContinueOnError)
	noText := fs.Bool("no-text", false, "skip text documents")
	noImages := fs.Bool("no-images", false, "skip preview images")
	render := fs.Bool("render", false, "render missing previews first")
	maxRender := fs.Int("max-render", 0, "cap renders (0 = all)")
	limit := fs.Int("limit", 0, "cap assets embedded per kind (0 = all pending)")
	classifyFlag := fs.Bool("classify", false, "fill empty categories from zero-shot guesses")
	apply := fs.Bool("apply", false, "with --classify: write the guesses (default: dry run)")
	margin := fs.Float64("margin", 0, "with --classify: confidence margin")
	eval := fs.Bool("eval", false, "report zero-shot accuracy against importer categories")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	if !d.Sem.Enabled() {
		return fmt.Errorf("semantic search is off: modon config set embedding.endpoint http://127.0.0.1:8178 (and embedding.autostart true, or run modon embed-server)")
	}
	startSidecar(d, 30*time.Minute)
	ctx := context.Background()

	if !(*noText && *noImages) {
		lastPrint := time.Time{}
		stats, err := d.Sem.Backfill(ctx, semantic.BackfillOptions{
			Text: !*noText, Images: !*noImages, Render: *render, MaxRender: *maxRender, Limit: *limit,
			Progress: func(stage string, done, total int) {
				if done == total || time.Since(lastPrint) > 5*time.Second {
					lastPrint = time.Now()
					fmt.Fprintf(os.Stderr, "%s %d/%d\n", stage, done, total)
				}
			},
		})
		if stats != nil {
			printJSON(stats)
		}
		if err != nil {
			return err
		}
	}
	if *eval {
		r, err := d.Sem.Eval(ctx)
		if err != nil {
			return err
		}
		printJSON(r)
	}
	if *classifyFlag {
		cs, err := d.Sem.AutoClassify(ctx, *apply, *margin)
		if err != nil {
			return err
		}
		printJSON(cs)
		if !*apply {
			fmt.Fprintln(os.Stderr, "dry run: add --apply to write these categories")
		}
	}
	return nil
}

func cmdEmbedServer(args []string) error {
	fs := flag.NewFlagSet("embed-server", flag.ContinueOnError)
	port := fs.Int("port", 0, "port (default from embedding.endpoint, else 8178)")
	device := fs.String("device", "", "cuda | cpu | mps (default auto)")
	imageTokens := fs.Int("image-tokens", 0, "vision tokens per image (70-1120)")
	textOnly := fs.Bool("text-only", false, "skip the vision encoder")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cache, err := config.CacheDir()
	if err != nil {
		return err
	}
	endpoint := cfg.Embedding.Endpoint
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8178"
	}
	if *port > 0 {
		endpoint = fmt.Sprintf("http://127.0.0.1:%d", *port)
	}
	o := embed.SidecarOptions{
		UV: cfg.Embedding.UVPath, CacheDir: cache, Endpoint: endpoint,
		ImageTokens: cfg.Embedding.ImageTokens, Device: cfg.Embedding.Device, TextOnly: *textOnly,
	}
	if *device != "" {
		o.Device = *device
	}
	if *imageTokens > 0 {
		o.ImageTokens = *imageTokens
	}
	cmd, err := embed.Command(context.Background(), o)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if cfg.Embedding.Endpoint == "" {
		fmt.Fprintf(os.Stderr, "tip: modon config set embedding.endpoint %s\n", endpoint)
	}
	return cmd.Run()
}

func cmdSimilar(args []string) error {
	fs := flag.NewFlagSet("similar", flag.ContinueOnError)
	category := fs.String("category", "", "")
	style := fs.String("style", "", "")
	limit := fs.Int("limit", 12, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: modon similar [--limit N] <id>")
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	startSidecar(d, 0)
	ctx := context.Background()
	ref, err := server.SimilarRef(ctx, d, fs.Arg(0))
	if err != nil {
		return err
	}
	assets, err := d.Sem.Similar(ctx, ref, model.Query{Category: *category, Style: *style, Limit: *limit})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(assets)
		return nil
	}
	fmt.Printf("owned assets similar to %s (%s):\n", ref.Asset.ID, ref.Asset.Title)
	for _, a := range assets {
		sim := "sim -"
		if a.Similarity > 0 {
			sim = fmt.Sprintf("sim %.3f", a.Similarity)
		}
		fmt.Printf("- %s | %s | %s | %s | %s\n", a.ID, a.Title, sim, strings.Join(a.MatchedBy, "+"), a.PrimaryFile)
	}
	return nil
}

func cmdSuggest(args []string) error {
	if len(args) < 1 || !strings.HasPrefix(args[0], "local:") {
		return fmt.Errorf("usage: modon suggest local:<id>")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "local:"), 10, 64)
	if err != nil {
		return err
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	startSidecar(d, 0)
	sg, err := d.Sem.Suggest(context.Background(), id)
	if err != nil {
		return err
	}
	printJSON(sg)
	return nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
