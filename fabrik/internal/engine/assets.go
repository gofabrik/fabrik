package engine

import (
	assetsdir "github.com/gofabrik/fabrik/assetmapper/directive"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/load"
)

// AssetTrees returns the //fabrik:assets trees declared in the module
// rooted at dir, in declaration order. Fatal diagnostics (a broken
// package, an invalid declaration) come back with a nil tree list.
func AssetTrees(dir string) ([]assetsdir.Tree, diag.Diagnostics, error) {
	res, err := load.Load(dir, nil)
	if err != nil {
		return nil, nil, err
	}
	diags := res.Diags
	if diags.HasFatal() {
		diags.Sort()
		return nil, diags, nil
	}
	assets := assetsdir.NewAssets(nil, nil, nil)
	for _, item := range res.Items {
		if item.Ann.Name != assets.Name() {
			continue
		}
		node, ds := assets.Parse(item.Ann)
		diags = append(diags, ds...)
		if node == nil || ds.HasFatal() {
			continue
		}
		diags = append(diags, assets.Check(node, item.Typed)...)
	}
	diags.Sort()
	if diags.HasFatal() {
		return nil, diags, nil
	}
	return assets.Trees(), diags, nil
}
