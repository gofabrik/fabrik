package directive

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/gen"
)

// typecheck compiles one source file and returns its package.
func typecheck(t *testing.T, src string) (*types.Package, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "config.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check("store", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	return pkg, fset
}

// register drives Parse and Check for one annotated struct.
func register(t *testing.T, c *Config, pkg *types.Package, fset *token.FileSet, section, typeName string) []string {
	t.Helper()
	n, ds := c.Parse(gen.Annotation{Name: "config", Args: section})
	if ds.HasFatal() {
		t.Fatalf("Parse(%q): %v", section, ds)
	}
	ds = c.Check(n, gen.Typed{Target: pkg.Scope().Lookup(typeName), Fset: fset})
	var msgs []string
	for _, d := range ds {
		msgs = append(msgs, d.Message)
	}
	return msgs
}

const keySrc = "package store\n\n" +
	"type Kind string\n\n" +
	"type TLS struct {\n" +
	"\tMode string `yaml:\"mode\"`\n" +
	"}\n\n" +
	"type Extra struct {\n" +
	"\tTimeout string `yaml:\"timeout\" default:\"30s\"`\n" +
	"}\n\n" +
	"type Config struct {\n" +
	"\tKind   Kind   `yaml:\"kind\" default:\"memory\"`\n" +
	"\tHidden string `yaml:\"-\"`\n" +
	"\tTLS    TLS    `yaml:\"tls\"`\n" +
	"\tExtra  Extra  `yaml:\",inline\"`\n" +
	"}\n"

func TestResolveKey(t *testing.T) {
	pkg, fset := typecheck(t, keySrc)
	c := New("config.yaml")
	if msgs := register(t, c, pkg, fset, "store", "Config"); len(msgs) > 0 {
		t.Fatalf("Check: %v", msgs)
	}

	tests := []struct {
		key  string
		ok   bool
		path string // dotted Go selector path
		def  string
	}{
		{key: "store.kind", ok: true, path: "Kind", def: "memory"},
		{key: "store.tls.mode", ok: true, path: "TLS.Mode"},
		{key: "store.timeout", ok: true, path: "Extra.Timeout", def: "30s"}, // ,inline flattens
		{key: "store.-", ok: false},                                         // yaml:"-" fields are invisible
		{key: "store.hidden", ok: false},                                    // the Go name is not a yaml key
		{key: "store.missing", ok: false},                                   // no such field
		{key: "other.kind", ok: false},                                      // no such section
	}
	for _, tt := range tests {
		nd, kf, ok := c.ResolveKey(tt.key)
		if ok != tt.ok {
			t.Errorf("ResolveKey(%q) ok = %v, want %v", tt.key, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if nd == nil {
			t.Errorf("ResolveKey(%q) returned nil node", tt.key)
		}
		if got := strings.Join(kf.Path, "."); got != tt.path {
			t.Errorf("ResolveKey(%q) path = %q, want %q", tt.key, got, tt.path)
		}
		if kf.Default != tt.def {
			t.Errorf("ResolveKey(%q) default = %q, want %q", tt.key, kf.Default, tt.def)
		}
	}
}

func TestSectionExclusivity(t *testing.T) {
	src := "package store\n\n" +
		"type Whole struct {\n\tAddr string `yaml:\"addr\"`\n}\n\n" +
		"type Part struct {\n\tKind string `yaml:\"kind\"`\n}\n"
	pkg, fset := typecheck(t, src)
	c := New("config.yaml")
	if msgs := register(t, c, pkg, fset, "", "Whole"); len(msgs) > 0 {
		t.Fatalf("sectionless Check: %v", msgs)
	}
	msgs := register(t, c, pkg, fset, "part", "Part")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "owns the whole file") {
		t.Fatalf("mixing sections = %v, want whole-file ownership error", msgs)
	}
}

func TestDottedSectionRejected(t *testing.T) {
	c := New("config.yaml")
	_, ds := c.Parse(gen.Annotation{Name: "config", Args: "store.primary"})
	if !ds.HasFatal() || !strings.Contains(ds[0].Message, "must not contain dots") {
		t.Fatalf("Parse(store.primary) = %v, want dotted-section error", ds)
	}
}

func TestMissingHint(t *testing.T) {
	pkg, fset := typecheck(t, keySrc)
	c := New("config.yaml")
	if msgs := register(t, c, pkg, fset, "store", "Config"); len(msgs) > 0 {
		t.Fatalf("Check: %v", msgs)
	}
	cfg := pkg.Scope().Lookup("Config").Type()
	if h, ok := c.MissingHint(cfg); !ok || !strings.Contains(h, "take *store.Config") {
		t.Fatalf("MissingHint(Config) = %q, %v; want take-a-pointer hint", h, ok)
	}
	if _, ok := c.MissingHint(pkg.Scope().Lookup("Kind").Type()); ok {
		t.Fatal("MissingHint(Kind) matched a non-config type")
	}
}
