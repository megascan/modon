package main

import (
	"errors"
	"flag"
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
