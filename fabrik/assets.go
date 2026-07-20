package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/assetmapper"
	"github.com/gofabrik/fabrik/fabrik/internal/diagfmt"
	"github.com/gofabrik/fabrik/fabrik/internal/engine"
)

// assetsCmd manages vendored packages for the app's asset trees.
func assetsCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fabrik assets <require|remove|prune> [args]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("assets "+sub, flag.ContinueOnError)
	jspm := fs.String("jspm", "", "jspm.io API mirror URL")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	dir, err := assetTreeDir()
	if err != nil {
		return err
	}
	imPath := filepath.Join(dir, assetmapper.ImportmapFilename)
	im, err := loadOrEmptyImportmap(imPath)
	if err != nil {
		return err
	}
	resolver := assetmapper.NewJSPMResolver(nil)
	resolver.BaseURL = *jspm
	v := &assetmapper.Vendor{
		Resolver:  resolver,
		VendorDir: filepath.Join(dir, assetmapper.VendorDir),
		Importmap: im,
	}

	switch sub {
	case "require":
		if fs.NArg() == 0 {
			return errors.New("usage: fabrik assets require <package>[@version] [package...]")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		// Orphaned files are recoverable; importmap entries for missing files are not.
		for _, arg := range fs.Args() {
			pkg, version := splitPackageVersion(arg)
			before := make(map[string]assetmapper.ImportmapEntry, len(im.Entries))
			for k, e := range im.Entries {
				before[k] = e
			}
			if err := v.Require(ctx, pkg, version); err != nil {
				return err
			}
			if err := im.Save(imPath); err != nil {
				return err
			}
			keys := make([]string, 0, len(im.Entries))
			for k := range im.Entries {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if prev, ok := before[k]; ok && prev == im.Entries[k] {
					continue
				}
				fmt.Printf("fabrik: vendored %s %s in %s\n", k, im.Entries[k].Version, dir)
			}
		}
		return nil

	case "remove":
		if fs.NArg() == 0 {
			return errors.New("usage: fabrik assets remove <specifier> [specifier...]")
		}
		// Preflight the batch before deleting any files.
		paths := make([]string, fs.NArg())
		for i, spec := range fs.Args() {
			p, err := v.ValidateRemove(spec)
			if err != nil {
				return err
			}
			paths[i] = p
		}
		// Persist entry removal before deleting files.
		for i, spec := range fs.Args() {
			delete(im.Entries, spec)
			if err := im.Save(imPath); err != nil {
				return err
			}
			if err := os.Remove(paths[i]); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Printf("fabrik: removed %s\n", spec)
		}
		fmt.Println("fabrik: run `fabrik assets prune` to delete orphaned transitive files")
		return nil

	case "prune":
		removed, err := v.Prune()
		if err != nil {
			return err
		}
		for _, rel := range removed {
			fmt.Printf("fabrik: pruned %s\n", rel)
		}
		if len(removed) == 0 {
			fmt.Println("fabrik: nothing to prune")
		}
		return nil

	default:
		return fmt.Errorf("unknown assets command %q (want require, remove, or prune)", sub)
	}
}

// assetTreeDir picks the declared asset tree that owns vendored packages.
func assetTreeDir() (string, error) {
	root, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	trees, diags, err := engine.AssetTrees(root)
	if err != nil {
		return "", err
	}
	if diags.HasFatal() {
		f := diagfmt.NewFormatter(os.Stderr)
		for _, d := range diags {
			if err := f.Emit(d); err != nil {
				return "", err
			}
		}
		if err := f.Summary(diags.Counts()); err != nil {
			return "", err
		}
		return "", errSilent
	}
	if len(trees) == 0 {
		return "", errors.New("no //fabrik:assets declaration found; declare an asset tree first")
	}
	var carrying []string
	for _, t := range trees {
		dir := filepath.Join(t.SrcDir, t.Dir)
		if _, err := os.Stat(filepath.Join(dir, assetmapper.ImportmapFilename)); err == nil {
			carrying = append(carrying, dir)
		}
	}
	switch {
	case len(carrying) == 1:
		return carrying[0], nil
	case len(carrying) > 1:
		return "", fmt.Errorf("importmap.json found in %s; only one asset tree may carry it", strings.Join(carrying, " and "))
	case len(trees) == 1:
		return filepath.Join(trees[0].SrcDir, trees[0].Dir), nil
	default:
		dirs := make([]string, len(trees))
		for i, t := range trees {
			dirs[i] = filepath.Join(t.SrcDir, t.Dir)
		}
		return "", fmt.Errorf("several asset trees and none carries importmap.json; create it in the tree that should own vendored packages: %s", strings.Join(dirs, ", "))
	}
}

// splitPackageVersion keeps scoped-package prefixes intact.
func splitPackageVersion(s string) (pkg, version string) {
	if at := strings.LastIndex(s, "@"); at > 0 {
		return s[:at], s[at+1:]
	}
	return s, ""
}

func loadOrEmptyImportmap(path string) (*assetmapper.Importmap, error) {
	im, err := assetmapper.LoadImportmap(path)
	if err == nil {
		return im, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return assetmapper.NewImportmap(), nil
	}
	return nil, err
}
