package templates

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/fstest"
)

// Only sets configured with Globals allocate binding pools.
func TestPoolsOnlyExistForGlobals(t *testing.T) {
	tree := fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"t/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}x{{ end }}`)},
		"t/mail/note.txt":         &fstest.MapFile{Data: []byte(`x`)},
	}

	plain, err := Load(tree, "t", Funcs(FuncMap{"unused": func() string { return "" }}))
	if err != nil {
		t.Fatal(err)
	}
	for name, entry := range plain.templates {
		if entry.bindings != nil {
			t.Errorf("%s: built a pool without globals", name)
		}
		if entry.tpl == nil {
			t.Errorf("%s: no template to render from", name)
		}
	}

	withGlobals, err := Load(tree, "t", Globals(func(*Binding) FuncMap {
		return FuncMap{"viewer": func() string { return "" }}
	}))
	if err != nil {
		t.Fatal(err)
	}
	for name, entry := range withGlobals.templates {
		if entry.bindings == nil {
			t.Errorf("%s: globals but no pool", name)
		}
		// The parsed tree must remain unexecuted so pooled clones can be created.
		if entry.tpl != nil {
			t.Errorf("%s: parsed tree still reachable for rendering", name)
		}
	}
}

// A pooled buffer must never carry one render's output into the next,
// including after a render that failed partway.
func TestPooledBufferDoesNotLeakBetweenRenders(t *testing.T) {
	tree := fstest.MapFS{
		"t/_default/_layout.html": &fstest.MapFile{Data: []byte(`{{ template "content" . }}`)},
		"t/_default/page.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}[{{ .Name }}]{{ end }}`)},
		"t/_default/boom.html":    &fstest.MapFile{Data: []byte(`{{ define "content" }}partial{{ fail }}{{ end }}`)},
	}
	set, err := Load(tree, "t", Funcs(FuncMap{
		"fail": func() (string, error) { return "", errors.New("no") },
	}))
	if err != nil {
		t.Fatal(err)
	}

	var long bytes.Buffer
	if err := set.Render(context.Background(), &long, "page", map[string]any{"Name": strings.Repeat("x", 4096)}); err != nil {
		t.Fatal(err)
	}
	if err := set.Render(context.Background(), io.Discard, "boom", nil); err == nil {
		t.Fatal("want the failing render to error")
	}

	var short bytes.Buffer
	if err := set.Render(context.Background(), &short, "page", map[string]any{"Name": "ada"}); err != nil {
		t.Fatal(err)
	}
	if short.String() != "[ada]" {
		t.Fatalf("render after a long and a failed one = %q", short.String())
	}
}

// An outsized buffer is dropped rather than pooled, so one large page
// cannot pin memory for the life of the process.
func TestOversizedBuffersAreNotPooled(t *testing.T) {
	large := &bytes.Buffer{}
	large.Grow(maxPooledBuffer + 1)
	putBuffer(large)
	for range 4 {
		if got := buffers.Get().(*bytes.Buffer); got == large {
			t.Fatal("an oversized buffer was returned to the pool")
		}
	}
}
