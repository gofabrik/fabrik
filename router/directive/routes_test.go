package directive

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func pos(line int) token.Position {
	return token.Position{Filename: "web/web.go", Line: line, Column: 1}
}

func TestRouteTable(t *testing.T) {
	rt := NewRouteTable()

	if d, ok := rt.add("GET /items", pos(1)); !ok {
		t.Fatalf("GET /items rejected: %v", d)
	}
	if d, ok := rt.add("POST /items", pos(2)); !ok {
		t.Fatalf("POST /items rejected: %v", d)
	}
	if d, ok := rt.add("GET /items/{id}", pos(3)); !ok {
		t.Fatalf("GET /items/{id} rejected: %v", d)
	}

	d, ok := rt.add("GET /items", pos(10))
	if ok || !strings.Contains(d.Message, "duplicate route GET /items") {
		t.Fatalf("duplicate = %+v, want duplicate-route error", d)
	}
	if !strings.Contains(d.Help, "web/web.go:1") {
		t.Fatalf("duplicate help = %q, want first declaration position", d.Help)
	}

	// Same structure with a different wildcard name is a ServeMux conflict,
	// not a duplicate; the diagnostic names the earlier pattern.
	d, ok = rt.add("GET /items/{name}", pos(11))
	if ok || !strings.Contains(d.Message, "conflicts with") {
		t.Fatalf("conflict = %+v, want conflict error", d)
	}
	if !strings.Contains(d.Message, "GET /items/{id}") {
		t.Fatalf("conflict message = %q, want the conflicting pattern named", d.Message)
	}
	if !strings.Contains(d.Help, "web/web.go:3") {
		t.Fatalf("conflict help = %q, want the earlier declaration position", d.Help)
	}
}

// typecheck compiles one source file and returns its package scope.
func typecheck(t *testing.T, src string) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "web.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check("web", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	return pkg
}

func TestSignatureChecks(t *testing.T) {
	pkg := typecheck(t, `package web

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) {}

func NotAHandler(w http.ResponseWriter) {}

func Middleware(next http.Handler) http.Handler { return next }

func NotMiddleware(next http.Handler) {}

func Generic[T any](next http.Handler) http.Handler { return next }

type Box[T any] struct{}

func (*Box[T]) Get(w http.ResponseWriter, r *http.Request) {}
`)
	fn := func(name string) *types.Func {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("missing func %s", name)
		}
		return obj.(*types.Func)
	}

	if !isHandlerSignature(fn("Handler").Signature()) {
		t.Error("Handler not recognized as handler")
	}
	if isHandlerSignature(fn("NotAHandler").Signature()) {
		t.Error("NotAHandler accepted as handler")
	}
	if !isMiddlewareSignature(fn("Middleware").Signature()) {
		t.Error("Middleware not recognized")
	}
	if isMiddlewareSignature(fn("NotMiddleware").Signature()) {
		t.Error("NotMiddleware accepted")
	}
	if !isGenericFunc(fn("Generic")) {
		t.Error("Generic not flagged as generic")
	}
	if isGenericFunc(fn("Handler")) {
		t.Error("Handler flagged as generic")
	}

	// A method on a generic receiver is generic via RecvTypeParams.
	box := pkg.Scope().Lookup("Box").Type().(*types.Named)
	get, _, _ := types.LookupFieldOrMethod(types.NewPointer(box), true, pkg, "Get")
	if get == nil {
		t.Fatal("missing method Box.Get")
	}
	if !isGenericFunc(get.(*types.Func)) {
		t.Error("method on generic receiver not flagged as generic")
	}
}
