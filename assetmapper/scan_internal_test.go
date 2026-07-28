package assetmapper

import (
	"slices"
	"strings"
	"testing"
)

func TestDiscoverVendorDependenciesUsesLexicalScanners(t *testing.T) {
	known := map[string]struct{}{
		"actual":    {},
		"commented": {},
		"string":    {},
	}

	js := []byte(`
const text = "import x from \"commented\"";
// import "commented";
/* export * from "commented"; */
import "actual";
`)
	if got := discoverVendorDependencies(js, "js", nil, known); !slices.Equal(got, []string{"actual"}) {
		t.Fatalf("JS dependencies = %v, want [actual]", got)
	}

	css := []byte(`
/* @import "commented"; */
body::before { content: "url(string)" }
@IMPORT URL("actual");
`)
	if got := discoverVendorDependencies(css, "css", nil, known); !slices.Equal(got, []string{"actual"}) {
		t.Fatalf("CSS dependencies = %v, want [actual]", got)
	}
}

func TestRewriteVendoredJSOnlyRewritesModuleSpecifiers(t *testing.T) {
	const upstream = "https://cdn.example/dependency.js"
	content := []byte(`
const text = "https://cdn.example/dependency.js";
// import "https://cdn.example/dependency.js";
import "https://cdn.example/dependency.js";
`)
	got := string(rewriteVendoredJS(content, map[string]string{upstream: "dependency"}))
	if strings.Count(got, upstream) != 2 {
		t.Fatalf("non-module URL was rewritten:\n%s", got)
	}
	if !strings.Contains(got, `import "dependency"`) {
		t.Fatalf("module specifier was not rewritten:\n%s", got)
	}

	escaped := []byte(`import "https:\u002f\u002fcdn.example/dependency.js";`)
	got = string(rewriteVendoredJS(escaped, map[string]string{upstream: "dependency"}))
	if got != `import "dependency";` {
		t.Fatalf("escaped upstream module URL was not rewritten: %s", got)
	}
}
