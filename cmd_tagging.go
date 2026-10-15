package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"modon/internal/semantic"
	"modon/internal/server"
	"modon/internal/store"
)

// ---- review queue / bulk tagging ----

func cmdUnclassified(args []string) error {
	fs := flag.NewFlagSet("unclassified", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "assets per page")
	offset := fs.Int("offset", 0, "skip this many (paging without claims)")
	worker := fs.String("worker", "", "claim the batch for this worker id")
	lease := fs.Duration("lease", store.DefaultLease, "claim duration")
	onePer := fs.Bool("one-per-cluster", false, "one representative per duplicate cluster")
	release := fs.Bool("release", false, "release the worker's claims")
	asJSON := fs.Bool("json", false, "JSON output (also for --release)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	if *release {
		n, err := d.St.ReleaseClaims(*worker)
		if err != nil {
			return err
		}
		if *asJSON {
			printJSON(map[string]any{"worker": *worker, "released": n})
		} else {
			fmt.Printf("released %d claim(s) of %s\n", n, *worker)
		}
		return nil
	}
	page, err := d.St.ListUnclassified(store.UnclassifiedOptions{
		Limit: *limit, Offset: *offset, Worker: *worker, Lease: *lease, OnePerCluster: *onePer,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(page)
		return nil
	}
	fmt.Printf("%d asset(s), %d still unclaimed in review", len(page.Assets), page.Remaining)
	if page.Worker != "" {
		fmt.Printf(", claimed for %s until %s", page.Worker, page.LeaseExpiresAt)
	}
	fmt.Println()
	for _, a := range page.Assets {
		dups := ""
		if len(a.Duplicates) > 0 {
			dups = fmt.Sprintf(" (+%d copies)", len(a.Duplicates))
		}
		fmt.Printf("- %s | %s%s | %s | %s | %s\n", a.ID, a.Title, dups, orDash(a.Category), strings.Join(a.Tags, " "), a.PrimaryFile)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	desc := fs.String("description", "", "description (agent-written)")
	category := fs.String("category", "", "canonical category")
	style := fs.String("style", "", "art-style descriptors")
	atype := fs.String("type", "", "model | animation | material | texture | other")
	tags := fs.String("tags", "", "comma-separated tags")
	mode := fs.String("tags-mode", "", "merge (default) | replace")
	remove := fs.String("remove-tags", "", "comma-separated tags to delete")
	dups := fs.Bool("duplicates", false, "also edit every copy in the duplicate cluster")
	rev := fs.Bool("reviewed", false, "force needs_review off")
	unrev := fs.Bool("unreviewed", false, "force needs_review on")
	batch := fs.String("batch", "", "JSON Lines file of edits (- = stdin); one re-embed pass at the end")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *batch != "" {
		if fs.NArg() > 0 {
			return fmt.Errorf("--batch takes its ids from the file; drop the positional ids")
		}
		return updateBatch(*batch, *asJSON)
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: modon update [flags] local:<id> [local:<id>...] | modon update --batch edits.jsonl")
	}
	u := store.MetadataUpdate{
		Description: *desc, Category: *category, Style: *style, AssetType: *atype,
		Tags: splitList(*tags), TagsMode: *mode, RemoveTags: splitList(*remove),
	}
	if *rev || *unrev {
		b := *rev
		u.Reviewed = &b
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	assets, err := server.UpdateMetadata(context.Background(), d, fs.Args(), u, *dups)
	if *asJSON {
		printJSON(assets)
	} else {
		for _, a := range assets {
			fmt.Printf("- %s | %s | %s | %s | needs_review=%v\n", a.ID, a.Title, orDash(a.Category), strings.Join(a.Tags, " "), a.NeedsReview)
		}
	}
	return err
}

// readBatch parses JSON Lines edits (blank lines and # comments skipped).
// A JSON array of edits is accepted too.
func readBatch(r io.Reader) ([]server.UpdateItem, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		var items []server.UpdateItem
		if err := json.Unmarshal(t, &items); err != nil {
			return nil, fmt.Errorf("batch array: %w", err)
		}
		return items, nil
	}
	var items []server.UpdateItem
	for n, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		var it server.UpdateItem
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields() // a typo like "tag" must not silently do nothing
		if err := dec.Decode(&it); err != nil {
			return nil, fmt.Errorf("batch line %d: %w", n+1, err)
		}
		items = append(items, it)
	}
	return items, nil
}

func updateBatch(path string, asJSON bool) error {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	items, err := readBatch(r)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("the batch has no edits")
	}
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	res := server.ApplyBatch(context.Background(), d, items)
	if asJSON {
		printJSON(res)
	} else {
		fmt.Printf("%d edit(s), %d asset(s) updated", res.Items, res.Updated)
		if e := res.Embedding; e != nil {
			fmt.Printf(", %d text vector(s) re-embedded", e.Text)
		} else {
			fmt.Print(", embedding server unavailable (vectors refresh on the next embed)")
		}
		fmt.Println()
		for _, e := range res.Errors {
			fmt.Printf("error: item %d %s: %s\n", e.Item, e.ID, e.Error)
		}
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("%d of %d edit(s) failed", len(res.Errors), res.Items)
	}
	return nil
}

func cmdDedupe(_ []string) error {
	d, err := openDeps()
	if err != nil {
		return err
	}
	defer d.Close()
	var ds *semantic.DedupeStats
	if d.Sem.Enabled() {
		ds, err = d.Sem.Dedupe()
	} else {
		ds, err = semantic.Dedupe(d.St, nil)
	}
	if err != nil {
		return err
	}
	printJSON(ds)
	return nil
}
