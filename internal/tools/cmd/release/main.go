// Command release runs repository release checks and operations.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gofabrik/fabrik/internal/tools/candidateproxy"
	"github.com/gofabrik/fabrik/internal/tools/converge"
	"github.com/gofabrik/fabrik/internal/tools/gittag"
	"github.com/gofabrik/fabrik/internal/tools/manifest"
	"github.com/gofabrik/fabrik/internal/tools/modset"
	"github.com/gofabrik/fabrik/internal/tools/workspace"
)

func main() {
	root := flag.String("root", ".", "repo root (dir with versions.yaml); default searches upward")
	flag.Parse()
	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	cfg, err := modset.Load(*root)
	if err != nil {
		fatal(err)
	}
	args := flag.Args()[1:]
	switch flag.Arg(0) {
	case "manifest-lint":
		findings, err := manifest.Analyze(cfg)
		if err != nil {
			fatal(err)
		}
		if len(findings) > 0 {
			for _, f := range findings {
				fmt.Fprintln(os.Stderr, f)
			}
			fmt.Fprintf(os.Stderr, "\nmanifest lint: %d finding(s); run `task manifest:fix`\n", len(findings))
			os.Exit(1)
		}
		fmt.Println("manifest lint: ok")
	case "manifest-fix":
		changed, err := manifest.Fix(cfg)
		if err != nil {
			fatal(err)
		}
		for _, m := range changed {
			fmt.Println("updated", m)
		}
		if len(changed) == 0 {
			fmt.Println("manifest fix: nothing to change")
		}
	case "workspace-sync":
		changed, err := workspace.Sync(cfg)
		if err != nil {
			fatal(err)
		}
		if changed {
			fmt.Println("go.work replaces regenerated at", cfg.Version)
		} else {
			fmt.Println("go.work replaces already current at", cfg.Version)
		}
	case "workspace-check":
		drift, err := workspace.Check(cfg)
		if err != nil {
			fatal(err)
		}
		if drift {
			fmt.Fprintln(os.Stderr, "go.work replaces are out of sync with versions.yaml; run `task workspace:sync`")
			os.Exit(1)
		}
		fmt.Println("go.work replaces in sync with", cfg.Version)
	case "assert-version":
		fs := flag.NewFlagSet("assert-version", flag.ExitOnError)
		version := fs.String("version", "", "expected module-set version (required)")
		_ = fs.Parse(args)
		if *version == "" {
			fatal(fmt.Errorf("assert-version: -version is required"))
		}
		if *version != cfg.Version {
			fatal(fmt.Errorf("versions.yaml declares %s, not %s", cfg.Version, *version))
		}
		fmt.Println("versions.yaml version is", cfg.Version)
	case "set-version":
		fs := flag.NewFlagSet("set-version", flag.ExitOnError)
		version := fs.String("version", "", "new module-set version (required)")
		_ = fs.Parse(args)
		if *version == "" {
			fatal(fmt.Errorf("set-version: -version is required"))
		}
		if err := modset.SetVersion(cfg.Root, *version); err != nil {
			fatal(err)
		}
		fmt.Println("versions.yaml version set to", *version)
	case "tag":
		fs := flag.NewFlagSet("tag", flag.ExitOnError)
		commit := fs.String("commit", "", "commit to tag (required)")
		remote := fs.String("remote", "origin", "remote to push tags to")
		push := fs.Bool("push", false, "push the created tags")
		_ = fs.Parse(args)
		if *commit == "" {
			fatal(fmt.Errorf("tag: -commit is required"))
		}
		created, err := gittag.Create(cfg, *commit, *remote, *push)
		if err != nil {
			fatal(err)
		}
		for _, t := range created {
			fmt.Println(t)
		}
		fmt.Printf("created %d tag(s) at %s (pushed=%v)\n", len(created), *commit, *push)
	case "build-proxy":
		if len(args) < 1 {
			usage()
			os.Exit(2)
		}
		revision := "HEAD"
		if len(args) >= 2 {
			revision = args[1]
		}
		if err := candidateproxy.Build(cfg, args[0], revision); err != nil {
			fatal(err)
		}
		fmt.Printf("candidate proxy for %s (%d modules) written to %s\n", cfg.Version, len(cfg.Published), args[0])
	case "converge":
		iters, err := converge.Run(cfg, 15)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("converged in %d iteration(s) and verified -mod=readonly at %s\n", iters, cfg.Version)
	case "verify":
		if err := converge.Verify(cfg); err != nil {
			fatal(err)
		}
		fmt.Printf("all modules build -mod=readonly against the candidate proxy at %s\n", cfg.Version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release [-root DIR] {manifest-lint|manifest-fix|workspace-sync|workspace-check|build-proxy OUT [REV]|converge|verify|assert-version -version V|set-version -version V|tag -commit SHA [-push]}")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "release:", err)
	os.Exit(1)
}
