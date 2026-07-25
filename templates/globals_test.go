package templates_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/templates"
)

type nameKey struct{}

func nameFrom(ctx context.Context) string {
	v, _ := ctx.Value(nameKey{}).(string)
	return v
}

func globalsTree(layout, page string) fstest.MapFS {
	return fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(layout)},
		"t/_default/page.html":    &fstest.MapFile{Data: []byte(page)},
	}
}

func viewerGlobals(b *templates.Binding) templates.FuncMap {
	return templates.FuncMap{
		"viewer": func() string { return nameFrom(b.Ctx()) },
	}
}

// Concurrent renders share one Set but must each read their own context.
func TestGlobalsIsolatePerRenderContext(t *testing.T) {
	set, err := templates.Load(globalsTree(`<p>{{ viewer }}</p>`, ``), "t",
		templates.Globals(viewerGlobals))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("visitor-%d", i)
			ctx := context.WithValue(context.Background(), nameKey{}, name)
			var out bytes.Buffer
			if err := set.Render(ctx, &out, "page", nil); err != nil {
				t.Error(err)
				return
			}
			if want := "<p>" + name + "</p>"; out.String() != want {
				t.Errorf("got %q, want %q", out.String(), want)
			}
		}()
	}
	wg.Wait()
}

// Reused clones preserve contextual HTML escaping.
func TestGlobalsOutputIsEscaped(t *testing.T) {
	layout := `<p>{{ viewer }}</p><a href="/u/{{ viewer }}">x</a>`
	set, err := templates.Load(globalsTree(layout, ``), "t",
		templates.Globals(viewerGlobals))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), nameKey{}, `a<script>&"`)
	for range 2 { // second render reuses a pooled, already-escaped clone
		var out bytes.Buffer
		if err := set.Render(ctx, &out, "page", nil); err != nil {
			t.Fatal(err)
		}
		if want := `<p>a&lt;script&gt;&amp;&#34;</p>`; !strings.Contains(out.String(), want) {
			t.Errorf("HTML context: %q lacks %q", out.String(), want)
		}
		if want := `/u/a%3cscript%3e&amp;%22`; !strings.Contains(out.String(), want) {
			t.Errorf("URL context: %q lacks %q", out.String(), want)
		}
	}
}

// Pooled bindings release render contexts before reuse.
func TestGlobalsClearContextOnReturn(t *testing.T) {
	var bind *templates.Binding
	set, err := templates.Load(globalsTree(`[{{ viewer }}]`, ``), "t",
		templates.Globals(func(b *templates.Binding) templates.FuncMap {
			bind = b
			return viewerGlobals(b)
		}))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ctx := context.WithValue(context.Background(), nameKey{}, "ada")
	if err := set.Render(ctx, &out, "page", nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[ada]" {
		t.Fatalf("got %q", out.String())
	}
	if bind == nil {
		t.Fatal("no binding was built")
	}
	if got := nameFrom(bind.Ctx()); got != "" {
		t.Errorf("returned binding still holds the render's context: %q", got)
	}
}

// A global returning an error aborts the render with the writer untouched.
func TestGlobalErrorLeavesWriterUntouched(t *testing.T) {
	set, err := templates.Load(globalsTree(`ok{{ boom }}tail`, ``), "t",
		templates.Globals(func(*templates.Binding) templates.FuncMap {
			return templates.FuncMap{
				"boom": func() (string, error) { return "", errors.New("global failed") },
			}
		}))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = set.Render(context.Background(), &out, "page", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "global failed") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("writer touched: %q", out.String())
	}
}

func TestGlobalsReachTextTemplates(t *testing.T) {
	tree := fstest.MapFS{
		"t/mail/_layout.txt": &fstest.MapFile{Data: []byte("{{ template \"content\" . }}\n")},
		"t/mail/note.txt":    &fstest.MapFile{Data: []byte("hi {{ viewer }}")},
	}
	set, err := templates.Load(tree, "t", templates.Globals(viewerGlobals))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ctx := context.WithValue(context.Background(), nameKey{}, "a<b")
	if err := set.Render(ctx, &out, "mail/note.txt", nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "hi a<b" {
		t.Fatalf("got %q", got)
	}
}

func TestFuncsOptionAppliesFunctions(t *testing.T) {
	set, err := templates.Load(globalsTree(`<p>{{ shout .Name }}</p>`, ``), "t",
		templates.Funcs(templates.FuncMap{"shout": strings.ToUpper}))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := set.Render(context.Background(), &out, "page", map[string]any{"Name": "ada"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "<p>ADA</p>" {
		t.Fatalf("got %q", out.String())
	}
}

// Builder contract violations become render errors.
func TestBuilderContractFailures(t *testing.T) {
	cases := []struct {
		name  string
		build func(*templates.Binding) templates.FuncMap
		want  string
	}{
		{
			name: "dropped name",
			build: func() func(*templates.Binding) templates.FuncMap {
				var calls int
				return func(b *templates.Binding) templates.FuncMap {
					calls++
					if calls > 1 {
						return templates.FuncMap{}
					}
					return viewerGlobals(b)
				}
			}(),
			want: "viewer",
		},
		{
			name: "changed signature",
			build: func() func(*templates.Binding) templates.FuncMap {
				var calls int
				return func(b *templates.Binding) templates.FuncMap {
					calls++
					if calls > 1 {
						return templates.FuncMap{"viewer": func() int { return 0 }}
					}
					return viewerGlobals(b)
				}
			}(),
			want: "viewer",
		},
		{
			name: "added name",
			build: func() func(*templates.Binding) templates.FuncMap {
				var calls int
				return func(b *templates.Binding) templates.FuncMap {
					calls++
					funcs := viewerGlobals(b)
					if calls > 1 {
						funcs["extra"] = func() string { return "" }
					}
					return funcs
				}
			}(),
			want: "extra",
		},
		{
			name: "panicking builder",
			build: func() func(*templates.Binding) templates.FuncMap {
				var calls int
				return func(b *templates.Binding) templates.FuncMap {
					calls++
					if calls > 1 {
						panic("builder exploded")
					}
					return viewerGlobals(b)
				}
			}(),
			want: "builder exploded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := templates.Load(globalsTree(`<p>{{ viewer }}</p>`, ``), "t",
				templates.Globals(tc.build))
			if err != nil {
				t.Fatal(err)
			}
			err = set.Render(context.Background(), io.Discard, "page", nil)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Ctx is non-nil during discovery and rendering.
func TestBindingContextIsNeverNil(t *testing.T) {
	var atBuild context.Context
	set, err := templates.Load(globalsTree(`[{{ viewer }}]`, ``), "t",
		templates.Globals(func(b *templates.Binding) templates.FuncMap {
			atBuild = b.Ctx()
			return viewerGlobals(b)
		}))
	if err != nil {
		t.Fatal(err)
	}
	if atBuild == nil {
		t.Error("builder saw a nil context")
	}
	var out bytes.Buffer
	if err := set.Render(context.Background(), &out, "page", nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]" {
		t.Fatalf("got %q", out.String())
	}
}

func TestGlobalsInvalidFuncMapFailsLoad(t *testing.T) {
	_, err := templates.Load(globalsTree(`x`, ``), "t",
		templates.Globals(func(*templates.Binding) templates.FuncMap {
			return templates.FuncMap{"bad": "not a function"}
		}))
	if err == nil {
		t.Fatal("want a Load error")
	}
}

func TestNilOptionIsIgnored(t *testing.T) {
	set, err := templates.Load(globalsTree(`<p>{{ .Name }}</p>`, ``), "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := set.Render(context.Background(), &out, "page", map[string]any{"Name": "ada"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "<p>ada</p>" {
		t.Fatalf("got %q", out.String())
	}
}

// A builder that fails on the discovery call fails Load, not the process.
func TestPanickingBuilderFailsLoad(t *testing.T) {
	_, err := templates.Load(globalsTree(`x`, ``), "t",
		templates.Globals(func(*templates.Binding) templates.FuncMap {
			panic("builder exploded")
		}))
	if err == nil {
		t.Fatal("want a Load error")
	}
	if !strings.Contains(err.Error(), "builder exploded") {
		t.Errorf("error %q does not carry the panic", err)
	}
}
