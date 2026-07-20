package assetmapper

import "html/template"

// FuncMap returns an [html/template] FuncMap with five helpers bound
// to m and im:
//
//   - asset "path" → string URL. html/template's context-aware
//     auto-escaping handles HTML attribute, URL attribute, and JS
//     contexts correctly because the helper returns a plain string,
//     not [template.HTML].
//   - importmap "entry"... → [template.HTML] containing the
//     <script type="importmap"> block, modulepreload links, and
//     entrypoint tags for the named entries.
//   - importmap_nonce "NONCE" "entry"... → same as importmap but
//     adds nonce="NONCE" to every <script> and <link>. Use under
//     Content-Security-Policy with script-src 'nonce-XYZ' /
//     style-src 'nonce-XYZ'.
//   - module_preload_links "entry"... → [template.HTML] containing
//     just the <link rel="modulepreload"> tags (use when composing
//     the importmap scaffolding manually).
//   - module_preload_links_nonce "NONCE" "entry"... → same as
//     module_preload_links with nonce attributes.
//
// All helpers return errors as a second value, which html/template
// surfaces as execution errors. Common cases: missing asset, typo'd
// entrypoint name, entry not marked as entrypoint.
//
// [Compiled.FuncMap] returns the same helpers bound to a [Build]
// result.
func FuncMap(m *Mapper, im *Importmap) template.FuncMap {
	return template.FuncMap{
		"asset": func(logicalPath string) (string, error) {
			return m.Asset(logicalPath)
		},
		"importmap": func(entrypoints ...string) (template.HTML, error) {
			s, err := im.Render(m, entrypoints...)
			if err != nil {
				return "", err
			}
			return template.HTML(s), nil // #nosec G203 -- output produced by the context-escaping importmap renderer
		},
		"importmap_nonce": func(nonce string, entrypoints ...string) (template.HTML, error) {
			s, err := im.RenderWithOptions(m, RenderOptions{
				Entrypoints: entrypoints,
				Nonce:       nonce,
			})
			if err != nil {
				return "", err
			}
			return template.HTML(s), nil // #nosec G203 -- output produced by the context-escaping importmap renderer
		},
		"module_preload_links": func(entrypoints ...string) (template.HTML, error) {
			s, err := im.ModulePreloadLinks(m, entrypoints...)
			if err != nil {
				return "", err
			}
			return template.HTML(s), nil // #nosec G203 -- output produced by the context-escaping importmap renderer
		},
		"module_preload_links_nonce": func(nonce string, entrypoints ...string) (template.HTML, error) {
			s, err := im.ModulePreloadLinksWithOptions(m, RenderOptions{
				Entrypoints: entrypoints,
				Nonce:       nonce,
			})
			if err != nil {
				return "", err
			}
			return template.HTML(s), nil // #nosec G203 -- output produced by the context-escaping importmap renderer
		},
	}
}
