package directive

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
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
func typecheckAs(t *testing.T, pkgPath, src string) *types.Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "pkg.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check(pkgPath, fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatalf("typecheck: %v", err)
	}
	return pkg
}

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

func TestCtorSignatureChecks(t *testing.T) {
	pkg := typecheck(t, `package web

import "net/http"

type MW func(http.Handler) http.Handler

type Dep struct{}

func Raw(d *Dep) func(http.Handler) http.Handler { return nil }

func Defined(d *Dep) MW { return nil }

func WithErr() (func(http.Handler) http.Handler, error) { return nil, nil }

func NoDeps() MW { return nil }

func WrongErr(d *Dep) (MW, string) { return nil, "" }

func NoResult(d *Dep) {}

func TwoMW() (MW, MW) { return nil, nil }

func Direct(next http.Handler) http.Handler { return next }
`)
	fn := func(name string) *types.Signature {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("missing func %s", name)
		}
		return obj.(*types.Func).Signature()
	}

	for _, name := range []string{"Raw", "WithErr"} {
		if !isCtorSignature(fn(name)) {
			t.Errorf("%s not recognized as constructor", name)
		}
	}
	// Defined func types other than router.Middleware are rejected:
	// generated code could not pass them to r.Use or route chains.
	for _, name := range []string{"Defined", "NoDeps", "WrongErr", "NoResult", "TwoMW"} {
		if isCtorSignature(fn(name)) {
			t.Errorf("%s accepted as constructor", name)
		}
	}

	// The forms are disjoint: a direct middleware returns
	// http.Handler, never a middleware-typed func.
	if !isMiddlewareSignature(fn("Direct")) {
		t.Error("Direct not recognized as direct middleware")
	}
	if isCtorSignature(fn("Direct")) {
		t.Error("Direct accepted as constructor")
	}

	mw := pkg.Scope().Lookup("MW").Type()
	if isMiddlewareType(mw) {
		t.Error("custom defined func type accepted - it cannot be passed as router.Middleware")
	}
	if isMiddlewareType(pkg.Scope().Lookup("Dep").Type()) {
		t.Error("struct accepted as middleware type")
	}

	if got := gen.LowerFirst("RequireAuth"); got != "requireAuth" {
		t.Errorf("lowerFirst = %q", got)
	}
	if got := gen.LowerFirst(""); got != "" {
		t.Errorf("lowerFirst empty = %q", got)
	}
}

func TestBundleDefersEmissionUntilRouterDemand(t *testing.T) {
	h := NewHost(NewGroup(), NewRouteTable(), NewMiddleware())
	g := gen.New()

	ds := h.EmitHandle(g, "/metrics/", pos(1), func() (string, diag.Diagnostics) {
		return "pkg.Metrics()", nil
	})
	if ds.HasFatal() {
		t.Fatalf("EmitHandle: %v", ds)
	}
	src, err := g.Render()
	if err != nil {
		t.Fatalf("render before demand: %v", err)
	}
	if strings.Contains(string(src), "/metrics/") {
		t.Fatalf("route emitted before router demand:\n%s", src)
	}

	r, rds := h.Router(g)
	if rds.HasFatal() {
		t.Fatalf("Router: %v", rds)
	}
	if r != "r" {
		t.Fatalf("router var = %q, want r", r)
	}
	src, err = g.Render()
	if err != nil {
		t.Fatalf("render after demand: %v", err)
	}
	if got := strings.Count(string(src), `"/metrics/"`); got != 1 {
		t.Fatalf("route emitted %d times after demand, want 1:\n%s", got, src)
	}

	if _, rds := h.Router(g); rds.HasFatal() {
		t.Fatalf("second Router: %v", rds)
	}
	if fds := h.FinishBundle(g); fds.HasFatal() {
		t.Fatalf("FinishBundle after build: %v", fds)
	}
	src, err = g.Render()
	if err != nil {
		t.Fatalf("render after re-trigger: %v", err)
	}
	if got := strings.Count(string(src), `"/metrics/"`); got != 1 {
		t.Fatalf("route emitted %d times after re-trigger, want 1:\n%s", got, src)
	}
}

func TestBundleFinishFallbackEmitsWithoutDemand(t *testing.T) {
	h := NewHost(NewGroup(), NewRouteTable(), NewMiddleware())
	g := gen.New()

	if ds := h.EmitHandle(g, "/metrics/", pos(1), func() (string, diag.Diagnostics) {
		return "pkg.Metrics()", nil
	}); ds.HasFatal() {
		t.Fatalf("EmitHandle: %v", ds)
	}
	if ds := h.FinishBundle(g); ds.HasFatal() {
		t.Fatalf("FinishBundle: %v", ds)
	}
	src, err := g.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := strings.Count(string(src), `"/metrics/"`); got != 1 {
		t.Fatalf("fallback emitted route %d times, want 1:\n%s", got, src)
	}
}

func TestBundleReplaysInRecordOrder(t *testing.T) {
	h := NewHost(NewGroup(), NewRouteTable(), NewMiddleware())
	g := gen.New()

	var got []string
	h.record(func(*gen.Gen) diag.Diagnostics { got = append(got, "first"); return nil })
	h.record(func(*gen.Gen) diag.Diagnostics { got = append(got, "second"); return nil })
	h.record(func(*gen.Gen) diag.Diagnostics { got = append(got, "third"); return nil })

	if ds := h.FinishBundle(g); ds.HasFatal() {
		t.Fatalf("FinishBundle: %v", ds)
	}
	want := []string{"first", "second", "third"}
	if len(got) != len(want) {
		t.Fatalf("replayed %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replay order = %v, want %v", got, want)
		}
	}
}

func TestBundleReplayResolvesServerWithoutCycle(t *testing.T) {
	h := NewHost(NewGroup(), NewRouteTable(), NewMiddleware())
	g := gen.New()
	pkg := typecheckAs(t, httpserverPath, "package httpserver\n\ntype Server struct{}\n")
	g.SetTypes(map[string]*types.Package{httpserverPath: pkg})
	srv := types.NewPointer(pkg.Scope().Lookup("Server").Type())

	h.BindHTTPServer(g)

	var nested string
	var nestedDS diag.Diagnostics
	h.record(func(g *gen.Gen) diag.Diagnostics {
		expr, ds, ok := g.Instance(srv, "")
		if !ok {
			t.Fatalf("server unresolvable during replay: %v", ds)
		}
		nested, nestedDS = expr, ds
		return ds
	})

	expr, ds, ok := g.Instance(srv, "")
	if !ok || ds.HasFatal() {
		t.Fatalf("Instance = %q ok=%v ds=%v", expr, ok, ds)
	}
	if nestedDS.HasFatal() {
		t.Fatalf("nested resolution reported diagnostics: %v", nestedDS)
	}
	if nested != expr {
		t.Fatalf("nested expr = %q, top-level %q; want the same server expression", nested, expr)
	}
	if !strings.Contains(expr, ".New(r, nil)") {
		t.Fatalf("server expr = %q, want httpserver.New(r, nil)", expr)
	}
}
