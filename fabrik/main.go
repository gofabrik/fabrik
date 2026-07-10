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
	case "assets":
		err = assetsCmd(args)
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
	fmt.Fprint(os.Stderr, `fabrik - build full-stack Go apps from //fabrik directives

Usage:
  fabrik <command> [args...]

Commands:
  new    <project>       Scaffold a new project
  wire   [-check] [dir]  Generate main.gen.go from directives
  assets <require|remove|prune>
                         Manage vendored JS packages in the asset tree
  run    [dir] [args]    Generate main.gen.go, then go run
  build  [dir] [-o out]  Generate main.gen.go, then go build
  directives             Print the directive reference (DIRECTIVES.md)
  lsp                    Run the language server (JSON-RPC over stdio)
  help                   Show this help

If [dir] is omitted, the current directory is used.
`)
}
