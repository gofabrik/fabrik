package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLSPMiddlewareCompletion verifies local and foreign middleware labels.
func TestLSPMiddlewareCompletion(t *testing.T) {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module app\n\ngo 1.26\n")
	write("main.go", "package main\n\nfunc main() { _ = run }\n")
	write("shared/mw.go", `package shared

import "net/http"

func RequireAuth(next http.Handler) http.Handler { return next }

func hidden(next http.Handler) http.Handler { return next }
`)
	webSrc := `package web

import "net/http"

func NoCache(next http.Handler) http.Handler { return next }

//fabrik:http GET /x middleware=
func X(w http.ResponseWriter, r *http.Request) {}
`
	write("web/web.go", webSrc)
	uri := uriFromFile(filepath.Join(dir, "web", "web.go"))

	c := startLSP(t)
	c.request(1, "initialize", map[string]any{})
	c.notifyServer("textDocument/didOpen", didOpenParams{
		TextDocument: textDocumentItem{URI: uri, LanguageID: "go", Version: 1, Text: webSrc},
	})

	items := completionResult(t, c.request(2, "textDocument/completion", completionParams{
		TextDocument: struct {
			URI string `json:"uri"`
		}{uri},
		Position: lspPosition{Line: 6, Character: len("//fabrik:http GET /x middleware=")},
	}))
	if !hasLabel(items, "NoCache") || !hasLabel(items, "shared.RequireAuth") {
		t.Fatalf("middleware completions = %+v, want NoCache and shared.RequireAuth", items)
	}
	for _, it := range items {
		if strings.Contains(it.Label, "hidden") {
			t.Fatalf("unexported foreign middleware offered: %+v", items)
		}
	}

	items = completionResult(t, c.request(3, "textDocument/completion", completionParams{
		TextDocument: struct {
			URI string `json:"uri"`
		}{uri},
		Position: lspPosition{Line: 6, Character: len("//fabrik:http GET /x middleware=")},
	}))
	if len(items) == 0 {
		t.Fatal("no completions for chained middleware")
	}

	c.request(4, "shutdown", nil)
	c.notifyServer("exit", nil)
}
