package assetmapper

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

// buildPlan is the validated, deterministic result shared by every compiled
// asset surface. It is complete before Build or Compile publishes any output.
type buildPlan struct {
	roots            []Root
	urlPrefix        string
	importmap        *Importmap
	assets           map[string]*plannedAsset
	order            []string
	manifest         *Manifest
	cspImportmapHash string
}

type plannedAsset struct {
	logical    string
	root       Root
	subPath    string
	kind       assetKind
	outputHash string
	etagHash   string
	output     string
	size       int64
	content    []byte
}

func planBuild(context string, roots []Root, im *Importmap, opts []BuildOption) (*buildPlan, error) {
	roots, err := normalizeRoots(context, roots)
	if err != nil {
		return nil, err
	}
	var cfg buildConfig
	for _, o := range opts {
		o(&cfg)
	}
	prefix, err := normalizeURLPrefix(cfg.urlPrefix)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}

	if im == nil {
		im, err = discoverImportmap(context, roots)
		if err != nil {
			return nil, err
		}
	}
	im = snapshotImportmap(im)

	collected, err := collectAssets(roots)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	if err := validateImportmap(context, im, collected); err != nil {
		return nil, err
	}
	canonicalizeImportmapPaths(im)

	plan := &buildPlan{
		roots:     roots,
		urlPrefix: prefix,
		importmap: im,
		assets:    make(map[string]*plannedAsset, len(collected)),
		manifest:  NewManifest(),
	}
	plan.manifest.URLPrefix = prefix
	plan.manifest.Dependencies = make(map[string][]string)

	hashedNames := make(map[string]string, len(collected))
	outputOwner := make(map[string]string, len(collected))

	var passthrough []string
	for logical, asset := range collected {
		if asset.kind != kindJS && asset.kind != kindCSS {
			passthrough = append(passthrough, logical)
		}
	}
	sort.Strings(passthrough)
	for _, logical := range passthrough {
		asset := collected[logical]
		hash, size, err := streamHash(asset.root.FS, asset.subPath)
		if err != nil {
			return nil, fmt.Errorf("%s: hash %s: %w", context, logical, err)
		}
		output := hashedName(logical, hash)
		if err := checkCollision(context, logical, output, collected, outputOwner); err != nil {
			return nil, err
		}
		outputOwner[output] = logical
		hashedNames[logical] = output
		plan.assets[logical] = &plannedAsset{
			logical:    logical,
			root:       asset.root,
			subPath:    asset.subPath,
			kind:       asset.kind,
			outputHash: hash,
			etagHash:   hash,
			output:     output,
			size:       size,
		}
		plan.order = append(plan.order, logical)
	}

	deps := make(map[string][]string)
	refsByAsset := make(map[string][]ref)
	var rewriteAssets []string
	for logical, asset := range collected {
		if asset.kind != kindJS && asset.kind != kindCSS {
			continue
		}
		rewriteAssets = append(rewriteAssets, logical)
	}
	sort.Strings(rewriteAssets)
	for _, logical := range rewriteAssets {
		asset := collected[logical]
		deps[logical] = nil
		refs := extractRefs(logical, asset.content, asset.kind)
		refsByAsset[logical] = refs
		for _, ref := range refs {
			if err := validatePlannedReference(context, logical, ref, im, collected, cfg.strictAssetURLs); err != nil {
				return nil, err
			}
			if ref.resolved == "" {
				continue
			}
			target, ok := collected[ref.resolved]
			if !ok || (target.kind != kindJS && target.kind != kindCSS) {
				continue
			}
			deps[logical] = appendUnique(deps[logical], ref.resolved)
		}
		plan.manifest.Dependencies[logical] = plannedDependencies(refs, im, collected)
	}

	for _, component := range dependencyComponents(deps) {
		if !component.cyclic {
			logical := component.nodes[0]
			asset := collected[logical]
			content := rewritePlannedContent(asset.content, refsByAsset[logical], prefix, hashedNames)
			hash := hashContent(content)
			output := hashedName(logical, hash)
			if err := checkCollision(context, logical, output, collected, outputOwner); err != nil {
				return nil, err
			}
			outputOwner[output] = logical
			hashedNames[logical] = output
			plan.assets[logical] = plannedRewriteAsset(logical, asset, hash, output, content)
			plan.order = append(plan.order, logical)
			continue
		}

		members := make(map[string]struct{}, len(component.nodes))
		for _, logical := range component.nodes {
			members[logical] = struct{}{}
		}
		hash := cyclicComponentHash(component.nodes, collected, refsByAsset, members, prefix, hashedNames)
		for _, logical := range component.nodes {
			output := hashedName(logical, hash)
			if err := checkCollision(context, logical, output, collected, outputOwner); err != nil {
				return nil, err
			}
			outputOwner[output] = logical
			hashedNames[logical] = output
		}
		for _, logical := range component.nodes {
			asset := collected[logical]
			content := rewritePlannedContent(asset.content, refsByAsset[logical], prefix, hashedNames)
			plan.assets[logical] = plannedRewriteAsset(logical, asset, hash, hashedNames[logical], content)
			plan.order = append(plan.order, logical)
		}
	}

	for logical, output := range hashedNames {
		plan.manifest.Entries[logical] = output
	}
	mapper := plan.mapper()
	plan.cspImportmapHash, err = im.importmapBodyHash(mapper)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	return plan, nil
}

func rewritePlannedContent(content []byte, refs []ref, prefix string, hashedNames map[string]string) []byte {
	if len(refs) == 0 {
		return content
	}
	return rewriteRefs(content, refs, func(ref ref) string {
		target, ok := hashedNames[ref.resolved]
		if !ok {
			return ref.spec
		}
		return prefix + target + ref.suffix
	})
}

func plannedRewriteAsset(
	logical string,
	asset *collectedAsset,
	hash, output string,
	content []byte,
) *plannedAsset {
	return &plannedAsset{
		logical:    logical,
		root:       asset.root,
		subPath:    asset.subPath,
		kind:       asset.kind,
		outputHash: hash,
		etagHash:   hashContent(content),
		output:     output,
		size:       int64(len(content)),
		content:    content,
	}
}

// cyclicComponentHash gives every member of a cycle one shared identity. Its
// canonical form includes member paths and source bytes, replaces internal
// references with stable logical markers, and uses finalized names for
// dependencies outside the component.
func cyclicComponentHash(
	nodes []string,
	assets map[string]*collectedAsset,
	refsByAsset map[string][]ref,
	members map[string]struct{},
	prefix string,
	hashedNames map[string]string,
) string {
	hasher := sha256.New()
	writeHashField(hasher, []byte("assetmapper-scc-v1"))
	writeHashField(hasher, []byte(prefix))
	for _, logical := range nodes {
		writeHashField(hasher, []byte(logical))
		writeHashField(hasher, assets[logical].content)
		content := rewriteRefs(assets[logical].content, refsByAsset[logical], func(ref ref) string {
			if _, internal := members[ref.resolved]; internal {
				return "\x00assetmapper-scc:" + ref.resolved + ref.suffix
			}
			target, ok := hashedNames[ref.resolved]
			if !ok {
				return ref.spec
			}
			return prefix + target + ref.suffix
		})
		writeHashField(hasher, content)
	}
	return hex.EncodeToString(hasher.Sum(nil))[:HashLength]
}

func writeHashField(hasher interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write(value)
}

func validatePlannedReference(
	context, importer string,
	ref ref,
	importmap *Importmap,
	assets map[string]*collectedAsset,
	strictAssetURLs bool,
) error {
	if ref.resolved != "" {
		if _, ok := assets[ref.resolved]; ok {
			return nil
		}
		strict := strictAssetURLs
		switch ref.kind {
		case referenceJSImport:
			strict = !strings.HasPrefix(ref.spec, "/") || strictAssetURLs
		case referenceCSSImport:
			strict = !strings.HasPrefix(ref.spec, "/") || strictAssetURLs
		case referenceCSSURL:
			strict = strictAssetURLs
		}
		if strict {
			return fmt.Errorf("%s: %s references missing asset %q", context, importer, ref.spec)
		}
		return nil
	}

	switch ref.kind {
	case referenceJSImport:
		if strings.HasPrefix(ref.spec, "./") || strings.HasPrefix(ref.spec, "../") {
			return fmt.Errorf("%s: %s references %q outside the asset roots", context, importer, ref.spec)
		}
		if strictAssetURLs && isRootRelativeReference(ref.spec) {
			return fmt.Errorf("%s: %s references invalid root-relative asset %q", context, importer, ref.spec)
		}
		if isBareJSImport(ref.spec) {
			if _, ok := importmap.Entries[ref.spec]; !ok {
				return fmt.Errorf("%s: %s imports bare specifier %q, which is absent from the importmap", context, importer, ref.spec)
			}
		}
	case referenceCSSImport:
		if strictAssetURLs && isRootRelativeReference(ref.spec) {
			return fmt.Errorf("%s: %s imports invalid root-relative asset %q", context, importer, ref.spec)
		}
		if isLocalReference(ref.spec) {
			return fmt.Errorf("%s: %s imports %q outside the asset roots", context, importer, ref.spec)
		}
	case referenceCSSURL:
		if strictAssetURLs && isRootRelativeReference(ref.spec) {
			return fmt.Errorf("%s: %s references invalid root-relative CSS URL %q", context, importer, ref.spec)
		}
		if strictAssetURLs && isLocalReference(ref.spec) {
			return fmt.Errorf("%s: %s references unresolved CSS URL %q", context, importer, ref.spec)
		}
	}
	return nil
}

func isBareJSImport(spec string) bool {
	return spec != "" &&
		!strings.HasPrefix(spec, "./") &&
		!strings.HasPrefix(spec, "../") &&
		!strings.HasPrefix(spec, "/") &&
		!strings.HasPrefix(spec, "//") &&
		!hasURLScheme(spec)
}

func isRootRelativeReference(spec string) bool {
	return strings.HasPrefix(spec, "/") && !strings.HasPrefix(spec, "//")
}

func (p *buildPlan) mapper() *Mapper {
	return &Mapper{roots: p.roots, urlPrefix: p.urlPrefix, manifest: p.manifest}
}

func canonicalizeImportmapPaths(im *Importmap) {
	for key, entry := range im.Entries {
		if entry.Path != "" {
			entry.Path = cleanLogical(entry.Path)
			im.Entries[key] = entry
		}
	}
}

func plannedDependencies(refs []ref, im *Importmap, assets map[string]*collectedAsset) []string {
	var deps []string
	for _, ref := range refs {
		logical := ref.resolved
		if logical == "" {
			entry, ok := im.Entries[ref.spec]
			if !ok {
				continue
			}
			logical = logicalForEntry(ref.spec, entry)
		}
		if _, ok := assets[logical]; ok {
			deps = appendUnique(deps, logical)
		}
	}
	return deps
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// checkCollision rejects output names that cannot be served unambiguously.
func checkCollision(context, logical, output string, assets map[string]*collectedAsset, outputOwner map[string]string) error {
	if other, dup := outputOwner[output]; dup {
		return fmt.Errorf("%s: %q and %q both compile to %q (hash collision or naming overlap); rename one of them",
			context, other, logical, output)
	}
	if _, shadowed := assets[output]; shadowed {
		return fmt.Errorf("%s: %q compiles to %q, which is also a literal source path; rename one of them",
			context, logical, output)
	}
	return nil
}

type collectedAsset struct {
	root    Root
	subPath string
	kind    assetKind
	content []byte
}

func collectAssets(roots []Root) (map[string]*collectedAsset, error) {
	assets := make(map[string]*collectedAsset)
	for _, root := range roots {
		err := fs.WalkDir(root.FS, ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || path == ImportmapFilename || path == VendorLockFilename ||
				path == VendorTransactionFilename {
				return nil
			}
			logical := path
			if root.MountAt != "" {
				logical = root.MountAt + "/" + path
			}
			if _, exists := assets[logical]; exists {
				return nil
			}
			kind := kindOf(logical)
			asset := &collectedAsset{root: root, subPath: path, kind: kind}
			if kind == kindJS || kind == kindCSS {
				content, err := fs.ReadFile(root.FS, path)
				if err != nil {
					return fmt.Errorf("read %s: %w", path, err)
				}
				asset.content = content
			}
			assets[logical] = asset
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk: %w", err)
		}
	}
	return assets, nil
}

func streamHash(fsys fs.FS, path string) (hash string, size int64, err error) {
	file, err := fsys.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close() //nolint:errcheck // read-only asset close cannot affect the completed hash
	hasher := sha256.New()
	size, err = io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil))[:HashLength], size, nil
}
