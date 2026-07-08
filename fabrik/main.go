package main

import (
	"errors"
	"fmt"
	"os"
)

// errSilent exits non-zero without an extra "fabrik:" prefix.
var errSilent = errors.New("silent")

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "new":
		err = newCmd(args)
	case "wire":
		err = wireCmd(args)
	case "run":
		err = runCmd(args)
	case "build":
		err = buildCmd(args)
	case "directives":
		err = directivesCmd(args)
	case "lsp":
		err = lspCmd(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, "fabrik:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fabrik — write handlers and providers, generate the wiring

Usage:
  fabrik <command> [args...]

Commands:
  new    <project>       Scaffold a new project
  wire   [-check] [dir]  Scan directives and generate main.gen.go
  run    [dir] [args]    Generate wiring, then go run
  build  [dir] [-o out]  Generate wiring, then go build
  directives             Print the directive reference (DIRECTIVES.md)
  lsp                    Run the language server (JSON-RPC over stdio)
  help                   Show this help

If [dir] is omitted, the current directory is used.
`)
}
