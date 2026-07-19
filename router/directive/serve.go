package directive

import (
	"fmt"
	"go/types"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// Serve infers an HTTP server from declared routes.
type Serve struct{ routes *routeTable }

// NewServe creates a Serve for routes.
func NewServe(routes *routeTable) *Serve { return &Serve{routes: routes} }

func (*Serve) Name() string { return "http:server" }

// Meta hides Serve from directive discovery and documentation.
func (*Serve) Meta() gen.Meta { return gen.Meta{Hidden: true} }

func (*Serve) Parse(gen.Annotation) (any, diag.Diagnostics) { return nil, nil }
func (*Serve) Check(any, gen.Typed) diag.Diagnostics        { return nil }
func (*Serve) Emit(any, *gen.Gen) diag.Diagnostics          { return nil }

// Finish emits an HTTP server entrypoint, preferring an injected *http.Server over the default PORT-based server.
func (s *Serve) Finish(g *gen.Gen) diag.Diagnostics {
	if !s.routes.HasRoutes() {
		return nil
	}
	var ds diag.Diagnostics
	r := g.Singleton(routerPath, "r", g.Import(routerPath)+".New()")
	g.RecordVarType(r, "*"+g.Import(routerPath)+".Router")
	httpPkg := g.Import("net/http")
	errPkg := g.Import("errors")
	ctxPkg := g.Import("context")
	timePkg := g.Import("time")
	ctx := g.Context()

	uses := []string{r}
	var body []string
	if t, ok := g.LookupType("net/http", "Server"); ok {
		if expr, sds, ok := g.Instance(types.NewPointer(t), ""); ok {
			ds = append(ds, sds...)
			body = append(body, "srv := "+expr)
			uses = append(uses, expr)
		}
	}
	if body == nil {
		osPkg := g.Import("os")
		body = append(body,
			`addr := ":8080"`,
			fmt.Sprintf(`if p := %s.Getenv("PORT"); p != "" {`, osPkg),
			`addr = ":" + p`,
			`}`,
			fmt.Sprintf("srv := &%s.Server{Addr: addr}", httpPkg),
		)
	}
	body = append(body,
		fmt.Sprintf("srv.Handler = %s", r),
		"errc := make(chan error, 1)",
		"go func() {",
		"if srv.TLSConfig != nil {",
		`errc <- srv.ListenAndServeTLS("", "")`,
		"} else {",
		"errc <- srv.ListenAndServe()",
		"}",
		"}()",
		"select {",
		"case err := <-errc:",
		fmt.Sprintf("if %s.Is(err, %s.ErrServerClosed) {", errPkg, httpPkg),
		"return nil",
		"}",
		"return err",
		fmt.Sprintf("case <-%s.Done():", ctx),
		"}",
		fmt.Sprintf("shutdownCtx, cancel := %s.WithTimeout(%s.Background(), 30*%s.Second)", ctxPkg, ctxPkg, timePkg),
		"defer cancel()",
		"return srv.Shutdown(shutdownCtx)",
	)
	g.AddEntrypoint("HTTPServer", uses, body)
	return ds
}
