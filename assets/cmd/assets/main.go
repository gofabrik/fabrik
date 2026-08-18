// Command assets manages vendored packages for an asset tree.
//
// Usage:
//
//	assets require [-dir assets] [-jspm url] <package>[@version]...
//	assets remove  [-dir assets] <specifier>...
//	assets prune   [-dir assets]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/assets"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "assets:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: assets <require|remove|prune> [-dir assets] [args]")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("assets "+sub, flag.ContinueOnError)
	dir := fs.String("dir", "assets", "asset tree directory (importmap.json at its top, files under vendor/)")
	jspm := fs.String("jspm", "", "jspm.io API mirror URL (default "+assets.DefaultJSPMBaseURL+")")
	allowHTTP := fs.Bool("allow-http", false, "allow HTTP for an explicitly trusted JSPM mirror")
	allowPrivate := fs.Bool("allow-private-network", false, "allow an explicitly trusted private-network JSPM mirror")
	allowCrossHost := fs.Bool("allow-cross-host-redirects", false, "allow JSPM redirects to another hostname")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if _, err := os.Stat(*dir); err != nil { // #nosec G703 -- validates the user-selected asset directory
		return fmt.Errorf("asset directory %q does not exist (pass -dir)", *dir)
	}

	imPath := filepath.Join(*dir, assets.ImportmapFilename)
	im, err := loadOrEmptyImportmap(imPath)
	if err != nil {
		return err
	}
	resolver := assets.NewJSPMResolver(nil)
	resolver.BaseURL = *jspm
	resolver.AllowHTTP = *allowHTTP
	resolver.AllowPrivateNetwork = *allowPrivate
	resolver.AllowCrossHostRedirects = *allowCrossHost
	v := &assets.Vendor{
		Resolver:      resolver,
		VendorDir:     filepath.Join(*dir, assets.VendorDir),
		Importmap:     im,
		ImportmapFile: imPath,
	}

	switch sub {
	case "require":
		if fs.NArg() == 0 {
			return errors.New("usage: assets require [-dir assets] <package>[@version] [more packages]")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		requests := make([]assets.PackageRequest, 0, fs.NArg())
		for _, arg := range fs.Args() {
			pkg, version := splitPackageVersion(arg)
			requests = append(requests, assets.PackageRequest{Name: pkg, Version: version})
		}
		before := entriesCopy(im)
		// Resolve and publish the command's complete request batch once.
		if err := v.RequirePackages(ctx, requests); err != nil {
			return err
		}
		reportChanged(out, im, before)
		return nil

	case "remove":
		if fs.NArg() == 0 {
			return errors.New("usage: assets remove [-dir assets] <specifier> [more specifiers]")
		}
		// Preflight the batch before deleting any files.
		for _, spec := range fs.Args() {
			if _, err := v.ValidateRemove(spec); err != nil {
				return err
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := v.RemovePackagesContext(ctx, fs.Args()); err != nil {
			return err
		}
		for _, spec := range fs.Args() {
			fmt.Fprintf(out, "removed %s\n", spec) //nolint:errcheck // CLI stdout status output is best-effort
		}
		fmt.Fprintln(out, "run `assets prune` to delete orphaned transitive files") //nolint:errcheck // CLI stdout status output is best-effort
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
	if before, after, found := strings.CutLast(s, "@"); found && before != "" {
		return before, after
	}
	return s, ""
}

func loadOrEmptyImportmap(path string) (*assets.Importmap, error) {
	im, err := assets.LoadImportmap(path)
	if err == nil {
		return im, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return assets.NewImportmap(), nil
	}
	return nil, err
}

func entriesCopy(im *assets.Importmap) map[string]assets.ImportmapEntry {
	out := make(map[string]assets.ImportmapEntry, len(im.Entries))
	maps.Copy(out, im.Entries)
	return out
}

// reportChanged prints added or updated entries in sorted order.
func reportChanged(out io.Writer, im *assets.Importmap, before map[string]assets.ImportmapEntry) {
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
