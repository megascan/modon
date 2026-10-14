package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"modon/internal/semantic"
	"modon/internal/server"
	"modon/internal/store"
)

// ---- review queue / bulk tagging ----

func cmdUnclassified(args []string) error {
	fs := flag.NewFlagSet("unclassified", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "")
	offset := fs.Int("offset", 0, "")
	worker := fs.String("worker", "", "claim the batch for this worker id")
	lease := fs.Duration("lease", store.DefaultLease, "claim duration")
	onePer := fs.Bool("one-per-cluster", false, "one representative per duplicate cluster")
	release := fs.Bool("release", false, "release the worker's claims")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
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
		fmt.Printf("released %d claim(s) of %s\n", n, *worker)
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
	desc := fs.String("description", "", "")
	category := fs.String("category", "", "")
	style := fs.String("style", "", "")
	atype := fs.String("type", "", "")
	tags := fs.String("tags", "", "comma-separated")
	mode := fs.String("tags-mode", "", "merge | replace")
	remove := fs.String("remove-tags", "", "comma-separated")
	dups := fs.Bool("duplicates", false, "also edit every copy in the duplicate cluster")
	rev := fs.Bool("reviewed", false, "")
	unrev := fs.Bool("unreviewed", false, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: modon update [flags] local:<id> [local:<id>...]")
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
