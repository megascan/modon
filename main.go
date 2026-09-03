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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"modon/internal/config"
	importdir "modon/internal/importer"
	"modon/internal/model"
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
	s := server.NewMCPServer(d)
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := model.Query{
		Text:      strings.Join(fs.Args(), " "),
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
		fmt.Printf("- %s | %s | %s | %s%s%s\n", a.ID, a.Title, price, a.Source, anim, flag)
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
	printJSON(map[string]any{"index": stats, "config": d.Cfg.Status()})
	return nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
