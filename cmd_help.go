package main

import (
	"flag"
	"fmt"
	"os"
)

// isHelpArg reports a request for help: -h, -help or --help.
func isHelpArg(s string) bool { return s == "-h" || s == "-help" || s == "--help" }

// wantsHelp reports whether args ask for help before any "--".
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpArg(a) {
			return true
		}
	}
	return false
}

// parseFlags parses a subcommand's flags. A help request prints the flag
// list to stdout and yields flag.ErrHelp, which main treats as success.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if wantsHelp(args) {
		fs.SetOutput(os.Stdout)
	}
	return fs.Parse(args)
}

// plainUsage is the usage of the subcommands that take no flags; they answer
// -h/--help with it (and exit 0) instead of treating it as an argument.
var plainUsage = map[string]string{
	"serve":   "modon [serve]                    run the MCP server over stdio",
	"details": "modon details <id>               full details (local:N, polypizza:ID, fab:UID)",
	"config":  "modon config [get <key> | set <key> <value> | path]",
	"service": "modon service install [--replace] | uninstall | status | print",
	"stats":   "modon stats                      local index statistics + config status",
	"dedupe":  "modon dedupe                     recompute duplicate clusters",
}

// printPlainUsage answers a help request for a flagless subcommand.
func printPlainUsage(cmd string, args []string) bool {
	u, ok := plainUsage[cmd]
	if !ok || len(args) == 0 || !isHelpArg(args[0]) {
		return false
	}
	fmt.Println("usage:", u)
	return true
}
