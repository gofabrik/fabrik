// Command assetmapper manages vendored packages for an asset tree.
//
// Usage:
//
//	assetmapper require [-dir assets] [-jspm url] <package>[@version]...
//	assetmapper remove  [-dir assets] <specifier>...
//	assetmapper prune   [-dir assets]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/assetmapper"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "assetmapper:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: assetmapper <require|remove|prune> [-dir assets] [args]")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("assetmapper "+sub, flag.ContinueOnError)
	dir := fs.String("dir", "assets", "asset tree directory (importmap.json at its top, files under vendor/)")
	jspm := fs.String("jspm", "", "jspm.io API mirror URL (default "+assetmapper.DefaultJSPMBaseURL+")")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if _, err := os.Stat(*dir); err != nil { // #nosec G703 -- validates the user-selected asset directory
		return fmt.Errorf("asset directory %q does not exist (pass -dir)", *dir)
	}

	imPath := filepath.Join(*dir, assetmapper.ImportmapFilename)
	im, err := loadOrEmptyImportmap(imPath)
	if err != nil {
		return err
	}
	resolver := assetmapper.NewJSPMResolver(nil)
	resolver.BaseURL = *jspm
	v := &assetmapper.Vendor{
		Resolver:  resolver,
		VendorDir: filepath.Join(*dir, assetmapper.VendorDir),
		Importmap: im,
	}

	switch sub {
	case "require":
		if fs.NArg() == 0 {
			return errors.New("usage: assetmapper require [-dir assets] <package>[@version] [more packages]")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		// Orphaned files are recoverable; importmap entries for missing files are not.
		for _, arg := range fs.Args() {
			pkg, version := splitPackageVersion(arg)
			before := entriesCopy(im)
			if err := v.Require(ctx, pkg, version); err != nil {
				return err
			}
			if err := im.Save(imPath); err != nil {
				return err
			}
			reportChanged(out, im, before)
		}
		return nil

	case "remove":
		if fs.NArg() == 0 {
			return errors.New("usage: assetmapper remove [-dir assets] <specifier> [more specifiers]")
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
			fmt.Fprintf(out, "removed %s\n", spec) //nolint:errcheck // CLI stdout status output is best-effort
		}
		fmt.Fprintln(out, "run `assetmapper prune` to delete orphaned transitive files") //nolint:errcheck // CLI stdout status output is best-effort
		return nil

	case "prune":
		removed, err := v.Prune()
		if err != nil {
			return err
		}
		for _, rel := range removed {
			fmt.Fprintf(out, "pruned %s\n", rel) //nolint:errcheck // CLI stdout status output is best-effort
		}
		if len(removed) == 0 {
			fmt.Fprintln(out, "nothing to prune") //nolint:errcheck // CLI stdout status output is best-effort
		}
		return nil

	default:
		return fmt.Errorf("unknown command %q (want require, remove, or prune)", sub)
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

func entriesCopy(im *assetmapper.Importmap) map[string]assetmapper.ImportmapEntry {
	out := make(map[string]assetmapper.ImportmapEntry, len(im.Entries))
	for k, e := range im.Entries {
		out[k] = e
	}
	return out
}

// reportChanged prints added or updated entries in sorted order.
func reportChanged(out io.Writer, im *assetmapper.Importmap, before map[string]assetmapper.ImportmapEntry) {
	keys := make([]string, 0, len(im.Entries))
	for k := range im.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := im.Entries[k]
		if prev, ok := before[k]; ok && prev == e {
			continue
		}
		fmt.Fprintf(out, "vendored %s %s\n", k, e.Version) //nolint:errcheck // CLI stdout status output is best-effort
	}
}
