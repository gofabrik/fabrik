package gen

import (
	"bytes"
	"go/types"
	"reflect"
	"testing"

	"github.com/gofabrik/fabrik/diag"
)

func TestDefinesAndUses(t *testing.T) {
	vars := map[string]bool{
		"webConfig": true, "webGreeter": true, "sharedConfig": true, "r": true,
	}

	sel := &Select{
		Var: "webGreeter", Iface: "web.Greeter",
		KeyExpr: "webConfig.Kind", FmtPkg: "fmt",
		Cases: []Case{{
			Value:  "hello",
			Result: Call{Fn: "web.NewHelloGreeter"},
		}},
	}
	if got := defines(sel); !reflect.DeepEqual(got, []string{"webGreeter"}) {
		t.Fatalf("defines(select) = %v", got)
	}
	if got := uses(sel, vars); !reflect.DeepEqual(got, []string{"webConfig"}) {
		t.Fatalf("uses(select) = %v, want [webConfig]", got)
	}

	lit := &StructLit{Var: "webAPI", Type: "web.API", Fields: []Field{{Name: "Greeter", Expr: "webGreeter"}}}
	if got := uses(lit, vars); !reflect.DeepEqual(got, []string{"webGreeter"}) {
		t.Fatalf("uses(structlit) = %v", got)
	}

	// Identifiers inside string literals must not count as uses.
	route := &Route{Router: "r", Kind: RouteMethod, Method: "GET", Pattern: "/webGreeter", Handler: "webAPI.Greet"}
	if got := uses(route, map[string]bool{"webGreeter": true, "r": true}); !reflect.DeepEqual(got, []string{"r"}) {
		t.Fatalf("uses(route) = %v, want [r] only - pattern string must not match", got)
	}

	// A node never uses what it defines; inline-if err calls define nothing.
	call := &Call{Fn: "shared.InitLogger", Args: []string{"sharedConfig"}, Err: ErrInline}
	if got := defines(call); got != nil {
		t.Fatalf("defines(inline call) = %v, want none", got)
	}
	if got := uses(call, vars); !reflect.DeepEqual(got, []string{"sharedConfig"}) {
		t.Fatalf("uses(call) = %v", got)
	}

	// Manual Uses entries participate.
	raw := &Raw{Base: Base{Uses: []string{"r"}}, Lines: []string{`addr := ":8080"`}, Defines: []string{"addr"}}
	if got := uses(raw, vars); !reflect.DeepEqual(got, []string{"r"}) {
		t.Fatalf("uses(raw) = %v, want manual [r]", got)
	}
}

func TestImportGroups(t *testing.T) {
	g := New()
	g.SetModule("demo")
	g.Import("net/http")
	g.Import("demo/web")
	g.Import("github.com/gofabrik/fabrik/router")
	g.Import("fmt")
	g.Import("demo/shared")

	var b bytes.Buffer
	g.writeImports(&b)
	want := `import (
"fmt"
"net/http"

"github.com/gofabrik/fabrik/router"

"demo/shared"
"demo/web"
)

`
	if b.String() != want {
		t.Fatalf("imports:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestLazyBindOwnerProvenance(t *testing.T) {
	g := New()
	g.SetDirective("config")
	g.BindLazy(types.Typ[types.String], "cfg", func() (string, diag.Diagnostics) {
		g.Stmt(PhaseConfig, "x := load()")
		return "x", nil
	})
	g.SetDirective("init") // the consumer materializes the binding
	if _, _, ok := g.Instance(types.Typ[types.String], "cfg"); !ok {
		t.Fatal("instance failed")
	}
	if got := g.nodes[0].base().Origin.Directive; got != "config" {
		t.Fatalf("lazy node directive = %q, want owner %q", got, "config")
	}
	if g.current != "init" {
		t.Fatalf("current directive = %q, want restored %q", g.current, "init")
	}
}
