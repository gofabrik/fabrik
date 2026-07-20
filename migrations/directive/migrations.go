// Package directive adds //fabrik:migrations to the generator.
package directive

import (
	"fmt"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
	"github.com/gofabrik/fabrik/migrations"
)

const migrationsPath = "github.com/gofabrik/fabrik/migrations"

const sourcesPath = migrationsPath + ".Sources"

// Migrations implements //fabrik:migrations.
type Migrations struct {
	decls      []*migNode
	registered bool
	treeFS     func(dir string) fs.FS
}

func NewMigrations() *Migrations {
	return &Migrations{
		treeFS: os.DirFS,
	}
}

// SetTreeFS lets validation see editor overlays.
func (mg *Migrations) SetTreeFS(f func(dir string) fs.FS) { mg.treeFS = f }

func (*Migrations) Name() string { return "migrations" }

func (*Migrations) Meta() gen.Meta {
	return gen.Meta{
		Synopsis: "Migration stream from an embedded tree: [dir=migrations] [stream=NAME]",
		Doc: "**`//fabrik:migrations [dir=migrations] [stream=NAME]`**\n\n" +
			"Declared on an exported `embed.FS` variable: the tree's " +
			"`NNNN_name.sql` files become one migration stream, bound with " +
			"every other declared stream into an injectable " +
			"`migrations.Sources`. Nothing runs automatically - call " +
			"`Sources.Migrate` from a `//fabrik:hook prepare` function, a " +
			"handler, or a command. Versions order within a stream. " +
			"Streams are independent, so tables that reference each other " +
			"belong in one stream. The default stream name is the package " +
			"path relative to the module. Use `stream=` to pin identity " +
			"when moving a package or declaring migrations at module root. " +
			"Use `all:<dir>` so embedded files match the validated " +
			"tree.\n\n" +
			"```go\n//fabrik:migrations\n//go:embed all:migrations\nvar Migrations embed.FS\n```",
		Example: "//fabrik:migrations",
		Attrs: []gen.AttrSpec{
			{Key: "dir", Kind: gen.KindFreeform},
			{Key: "stream", Kind: gen.KindFreeform},
		},
		Tier: gen.TierBind,
	}
}

type migNode struct {
	pos    token.Position
	dir    string
	stream string

	varName  string
	pkg      *types.Package
	srcDir   string
	resolved string
	built    bool
}

func (mg *Migrations) Parse(a gen.Annotation) (any, diag.Diagnostics) {
	args, ds := gen.ParseArgs(a, mg.Meta())
	nd := &migNode{pos: a.Pos, dir: "migrations"}
	if d, ok := args.Attr["dir"]; ok {
		nd.dir = d.Text
		if err := validStream(nd.dir); err != nil {
			ds.Error(a.ArgPos(d.Col), fmt.Sprintf("invalid dir: %v", err),
				"use a clean relative path: dir=migrations")
		}
	}
	if m, ok := args.Attr["stream"]; ok {
		nd.stream = m.Text
		if err := validStream(nd.stream); err != nil {
			ds.Error(a.ArgPos(m.Col), fmt.Sprintf("invalid stream name: %v", err),
				"use clean slash-separated segments: stream=auth, stream=internal/billing")
		}
	}
	checkEmbedPattern(a, nd.dir, &ds)
	if ds.HasFatal() {
		return nil, ds
	}
	return nd, ds
}

func validStream(m string) error {
	if m == "" {
		return fmt.Errorf("empty name")
	}
	if strings.HasPrefix(m, "/") || strings.HasSuffix(m, "/") {
		return fmt.Errorf("%q has a leading or trailing slash", m)
	}
	for _, seg := range strings.Split(m, "/") {
		switch seg {
		case "", ".", "..":
			return fmt.Errorf("%q has a %q segment", m, seg)
		}
	}
	return nil
}

// checkEmbedPattern requires all: so validation sees every shipped migration.
func checkEmbedPattern(a gen.Annotation, dir string, ds *diag.Diagnostics) {
	found, covered := gen.EmbedCovers(a, "all:"+dir)
	if covered {
		return
	}
	if !found {
		ds.Error(a.Pos, "//fabrik:migrations variable has no //go:embed",
			fmt.Sprintf("add //go:embed all:%s above the variable", dir))
		return
	}
	ds.Error(a.Pos, fmt.Sprintf("the go:embed pattern does not cover all:%s", dir),
		fmt.Sprintf("use //go:embed all:%s - a plain pattern can silently omit migration files, and a migration that never ships never runs", dir))
}

func (mg *Migrations) Check(n any, ty gen.Typed) diag.Diagnostics {
	nd := n.(*migNode)
	var ds diag.Diagnostics

	v, ok := ty.Target.(*types.Var)
	if !ok {
		ds.Error(nd.pos, "//fabrik:migrations must be on a package-level variable", "")
		return ds
	}
	if !v.Exported() {
		ds.Error(nd.pos, fmt.Sprintf("migrations variable %s is unexported", v.Name()),
			"generated code lives in package main; export the variable")
		return ds
	}
	if types.TypeString(types.Unalias(v.Type()), nil) != "embed.FS" {
		ds.Error(nd.pos, fmt.Sprintf("migrations variable %s is not an embed.FS", v.Name()),
			"example: //go:embed all:migrations\nvar Migrations embed.FS")
		return ds
	}
	nd.varName = v.Name()
	nd.pkg = v.Pkg()
	nd.srcDir = filepath.Dir(nd.pos.Filename)
	mg.decls = append(mg.decls, nd)
	return ds
}

// resolveStreams assigns stable stream names before validation or emission.
func (mg *Migrations) resolveStreams(g *gen.Gen) diag.Diagnostics {
	var ds diag.Diagnostics
	for _, d := range mg.decls {
		if d.resolved != "" {
			continue
		}
		if d.stream != "" {
			d.resolved = d.stream
			continue
		}
		rel := strings.TrimPrefix(d.pkg.Path(), g.Module()+"/")
		if rel == g.Module() || rel == "" || rel == "." {
			ds.Error(d.pos, "cannot derive a stream name for a package at the module root",
				"name the stream explicitly: //fabrik:migrations stream=NAME")
			continue
		}
		d.resolved = rel
	}
	return ds
}

// Emit binds one migrations.Sources for all declared streams.
func (mg *Migrations) Emit(n any, g *gen.Gen) diag.Diagnostics {
	nd := n.(*migNode)
	if nd.varName == "" || mg.registered {
		return nil
	}
	mg.registered = true
	g.BindLazyPath(sourcesPath, func() (string, diag.Diagnostics) {
		ds := mg.resolveStreams(g)
		if ds.HasFatal() {
			return "", ds
		}
		decls := mg.sortedByStream()
		for _, d := range decls {
			d.built = true
		}
		migPkg := g.Import(migrationsPath)
		var b strings.Builder
		b.WriteString(migPkg + ".Sources{\n")
		for _, d := range decls {
			fmt.Fprintf(&b, "{Stream: %q, FS: %s.%s, Dir: %q},\n", d.resolved, g.ImportPkg(d.pkg), d.varName, d.dir)
		}
		b.WriteString("}")
		v := g.Var("migrationSources")
		g.Node(&gen.Assign{
			Base: gen.Base{Phase: gen.PhaseWire, Origin: gen.Origin{Pos: decls[0].pos}},
			Var:  v,
			Expr: b.String(),
		})
		return v, ds
	})
	return nil
}

func (mg *Migrations) sortedByStream() []*migNode {
	decls := append([]*migNode(nil), mg.decls...)
	sort.Slice(decls, func(i, j int) bool { return decls[i].resolved < decls[j].resolved })
	return decls
}

// Validate checks stream names and migration trees.
func (mg *Migrations) Validate(g *gen.Gen) diag.Diagnostics {
	if len(mg.decls) == 0 {
		return nil
	}
	ds := mg.resolveStreams(g)
	if ds.HasFatal() {
		return ds
	}

	owner := map[string]*migNode{}
	for _, d := range mg.decls {
		if first, dup := owner[d.resolved]; dup {
			ds.Error(d.pos, fmt.Sprintf("migration stream %q is already declared at %s", d.resolved, first.pos),
				"streams are bookkeeping identity; name one of them differently with stream=")
			continue
		}
		owner[d.resolved] = d
	}

	for _, d := range mg.decls {
		src := migrations.Sources{{Stream: d.resolved, FS: mg.treeFS(d.srcDir), Dir: d.dir}}
		if err := src.Check(); err != nil {
			ds.Error(d.pos, err.Error(),
				"migrations are validated at generation time; fix the tree and rerun")
		}
	}

	for _, d := range mg.decls {
		if !d.built {
			ds.Warn(d.pos, fmt.Sprintf("migrations %s can never run: nothing injects migrations.Sources", d.varName),
				"call them from a prepare hook: //fabrik:hook prepare\nfunc MigrateDB(ctx context.Context, db *sql.DB, d migrations.Dialect, src migrations.Sources) error { return src.Migrate(ctx, db, d) }")
		}
	}
	return ds
}

// MissingHint explains migration-specific injected types.
func (mg *Migrations) MissingHint(ty types.Type) (string, bool) {
	switch types.TypeString(types.Unalias(ty), nil) {
	case sourcesPath:
		return "declare a migration tree: //fabrik:migrations on an embedded directory of NNNN_name.sql files", true
	case migrationsPath + ".Dialect":
		return "provide it next to your database: //fabrik:provider\nfunc NewDialect(cfg *DBConfig) migrations.Dialect { return migrations.DialectSQLite }", true
	}
	return "", false
}
