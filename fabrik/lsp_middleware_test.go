package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLSPMiddlewareCompletion verifies declared middleware names complete.
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

//fabrik:http:middleware name=auth
func RequireAuth(next http.Handler) http.Handler { return next }

//fabrik:http:middleware name=No.Cache
func Rejected(next http.Handler) http.Handler { return next }

func unnamed(next http.Handler) http.Handler { return next }
`)
	webSrc := `package web

import "net/http"

//fabrik:http:middleware name=nocache
func NoCache(next http.Handler) http.Handler { return next }

//fabrik:http GET /x middleware=nocache, au
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
		Position: lspPosition{Line: 7, Character: len("//fabrik:http GET /x middleware=")},
	}))
	if !hasLabel(items, "nocache") || !hasLabel(items, "auth") {
		t.Fatalf("middleware completions = %+v, want nocache and auth", items)
	}
	for _, it := range items {
		if strings.Contains(it.Label, "unnamed") || strings.Contains(it.Label, "RequireAuth") || strings.Contains(it.Label, "No.Cache") {
			t.Fatalf("undeclared or invalid middleware offered: %+v", items)
		}
	}

	// After a comma and a space, the chain continues.
	items = completionResult(t, c.request(3, "textDocument/completion", completionParams{
		TextDocument: struct {
			URI string `json:"uri"`
		}{uri},
		Position: lspPosition{Line: 7, Character: len("//fabrik:http GET /x middleware=nocache, ")},
	}))
	if !hasLabel(items, "auth") {
		t.Fatalf("chained completions after comma+space = %+v, want auth", items)
	}

	// A typed partial after comma+space filters by prefix.
	items = completionResult(t, c.request(4, "textDocument/completion", completionParams{
		TextDocument: struct {
			URI string `json:"uri"`
		}{uri},
		Position: lspPosition{Line: 7, Character: len("//fabrik:http GET /x middleware=nocache, au")},
	}))
	if !hasLabel(items, "auth") || hasLabel(items, "nocache") {
		t.Fatalf("partial completions after comma+space = %+v, want auth only", items)
	}

	c.request(5, "shutdown", nil)
	c.notifyServer("exit", nil)
}
