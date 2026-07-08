package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type lspClient struct {
	t       *testing.T
	in      io.Writer
	replies chan rpcMessage
	notes   chan rpcMessage
}

func startLSP(t *testing.T) *lspClient {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := newLSPServer()
	go s.run(inR, outW)
	t.Cleanup(func() { inW.Close() })

	c := &lspClient{t: t, in: inW, replies: make(chan rpcMessage, 16), notes: make(chan rpcMessage, 64)}
	go func() {
		r := bufio.NewReader(outR)
		for {
			msg, err := readMessage(r)
			if err != nil {
				return
			}
			if msg.Method != "" {
				c.notes <- *msg
			} else {
				c.replies <- *msg
			}
		}
	}()
	return c
}

func (c *lspClient) request(id int, method string, params any) json.RawMessage {
	c.t.Helper()
	body, _ := json.Marshal(params)
	rawID, _ := json.Marshal(id)
	if err := writeMessage(c.in, rpcMessage{JSONRPC: "2.0", ID: rawID, Method: method, Params: body}); err != nil {
		c.t.Fatal(err)
	}
	select {
	case msg := <-c.replies:
		return msg.Result
	case <-time.After(10 * time.Second):
		c.t.Fatalf("no reply to %s", method)
		return nil
	}
}

func (c *lspClient) notifyServer(method string, params any) {
	c.t.Helper()
	body, _ := json.Marshal(params)
	if err := writeMessage(c.in, rpcMessage{JSONRPC: "2.0", Method: method, Params: body}); err != nil {
		c.t.Fatal(err)
	}
}

// diagnostics waits for the next publishDiagnostics for uri.
func (c *lspClient) diagnostics(uri string) publishDiagnosticsParams {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-c.notes:
			if msg.Method != "textDocument/publishDiagnostics" {
				continue
			}
			var p publishDiagnosticsParams
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				c.t.Fatal(err)
			}
			if p.URI == uri {
				return p
			}
		case <-deadline:
			c.t.Fatalf("no diagnostics published for %s", uri)
		}
	}
}

func TestLSP(t *testing.T) {
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
	write("main.go", "package main\n\nfunc main() {\n\tif err := run(); err != nil {\n\t\tpanic(err)\n\t}\n}\n")
	webSrc := `package web

import "net/http"

//fabrik:http GET
func Index(w http.ResponseWriter, r *http.Request) {}
`
	write("web/web.go", webSrc)
	uri := uriFromFile(filepath.Join(dir, "web", "web.go"))

	c := startLSP(t)
	c.request(1, "initialize", map[string]any{})
	c.notifyServer("initialized", map[string]any{})
	c.notifyServer("textDocument/didOpen", didOpenParams{
		TextDocument: textDocumentItem{URI: uri, LanguageID: "go", Version: 1, Text: webSrc},
	})

	p := c.diagnostics(uri)
	if len(p.Diagnostics) != 1 || !strings.Contains(p.Diagnostics[0].Message, "requires PATH after METHOD") {
		t.Fatalf("tier-1 diagnostics = %+v, want missing-PATH error", p.Diagnostics)
	}

	p = c.diagnostics(uri)
	if len(p.Diagnostics) != 1 {
		t.Fatalf("tier-2 diagnostics = %+v, want 1", p.Diagnostics)
	}

	fixed := strings.Replace(webSrc, "//fabrik:http GET", "//fabrik:http GET /", 1)
	c.notifyServer("textDocument/didChange", didChangeParams{
		TextDocument:   versionedTextDocumentIdentifier{URI: uri, Version: 2},
		ContentChanges: []textDocumentContentChangeEvent{{Text: fixed}},
	})
	p = c.diagnostics(uri)
	if len(p.Diagnostics) != 0 {
		t.Fatalf("diagnostics after fix = %+v, want none", p.Diagnostics)
	}

	items := completionResult(t, c.request(2, "textDocument/completion", completionParams{
		TextDocument: struct {
			URI string `json:"uri"`
		}{uri},
		Position: lspPosition{Line: 4, Character: len("//fabrik:")},
	}))
	if !hasLabel(items, "http") || !hasLabel(items, "provider") {
		t.Fatalf("directive completions = %+v, want http and provider", items)
	}

	items = completionResult(t, c.request(3, "textDocument/completion", completionParams{
		TextDocument: struct {
			URI string `json:"uri"`
		}{uri},
		Position: lspPosition{Line: 4, Character: len("//fabrik:http ")},
	}))
	if !hasLabel(items, "GET") || !hasLabel(items, "DELETE") {
		t.Fatalf("method completions = %+v, want HTTP methods", items)
	}

	var hov hoverResult
	raw := c.request(4, "textDocument/hover", hoverParams{
		TextDocument: struct {
			URI string `json:"uri"`
		}{uri},
		Position: lspPosition{Line: 4, Character: len("//fabrik:h")},
	})
	if err := json.Unmarshal(raw, &hov); err != nil || !strings.Contains(hov.Contents.Value, "fabrik:http") {
		t.Fatalf("hover = %s (err %v), want fabrik:http doc", raw, err)
	}

	c.request(5, "shutdown", nil)
	c.notifyServer("exit", nil)
}

func completionResult(t *testing.T, raw json.RawMessage) []completionItem {
	t.Helper()
	var items []completionItem
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("completion result %s: %v", raw, err)
	}
	return items
}

func hasLabel(items []completionItem, label string) bool {
	for _, it := range items {
		if it.Label == label {
			return true
		}
	}
	return false
}
