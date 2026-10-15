package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestHelpIsNotAnError(t *testing.T) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.Bool("json", false, "")
	for _, arg := range []string{"-h", "-help", "--help"} {
		if err := parseFlags(fs, []string{arg}); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s: err = %v, want flag.ErrHelp (main exits 0 on it)", arg, err)
		}
	}
	if !printPlainUsage("details", []string{"--help"}) || printPlainUsage("details", []string{"local:1"}) {
		t.Fatal("flagless commands answer --help, and only --help")
	}
}

func TestReadBatch(t *testing.T) {
	in := `# comment
{"id":"local:1","category":"container","tags":["crate","wooden"],"tags_mode":"replace","apply_to_duplicates":true}

{"ids":["local:2","local:3"],"description":"Stone wall segment","reviewed":true}
`
	items, err := readBatch(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "local:1" || len(items[0].Tags) != 2 || !items[0].ApplyToDuplicates ||
		len(items[1].IDs) != 2 || items[1].Reviewed == nil || !*items[1].Reviewed {
		t.Fatalf("items = %+v", items)
	}
	if items, err := readBatch(strings.NewReader(`[{"id":"local:4","category":"rock"}]`)); err != nil || len(items) != 1 {
		t.Fatalf("array form: %v %+v", err, items)
	}
	if _, err := readBatch(strings.NewReader(`{"id":"local:1","tag":["oops"]}`)); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("unknown fields must fail with the line number, got %v", err)
	}
}
