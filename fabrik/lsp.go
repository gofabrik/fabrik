package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// lspCmd runs the directive-only LSP server over stdio.
func lspCmd(args []string) error {
	fs := flag.NewFlagSet("lsp", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	s := newLSPServer()
	return s.run(os.Stdin, os.Stdout)
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func readMessage(r *bufio.Reader) (*rpcMessage, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &contentLength); err != nil {
				return nil, err
			}
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var msg rpcMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func writeMessage(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

type lspPosition struct {
	Line      int `json:"line"`      // 0-based
	Character int `json:"character"` // 0-based
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspDiagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity"` // 1=Error, 2=Warning
	Source   string   `json:"source"`
	Message  string   `json:"message"`
}

type publishDiagnosticsParams struct {
	URI         string          `json:"uri"`
	Diagnostics []lspDiagnostic `json:"diagnostics"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type textDocumentContentChangeEvent struct {
	Range *lspRange `json:"range,omitempty"`
	Text  string    `json:"text"`
}

type didChangeParams struct {
	TextDocument   versionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []textDocumentContentChangeEvent `json:"contentChanges"`
}

type didSaveParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Text *string `json:"text,omitempty"`
}

type didCloseParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
}

type completionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position lspPosition `json:"position"`
}

type completionItem struct {
	Label      string `json:"label"`
	Kind       int    `json:"kind,omitempty"`   // 14=Keyword, 5=Field, 12=Value
	Detail     string `json:"detail,omitempty"` // shown on the right
	InsertText string `json:"insertText,omitempty"`
}

type hoverParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position lspPosition `json:"position"`
}

type markupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type hoverResult struct {
	Contents markupContent `json:"contents"`
	Range    *lspRange     `json:"range,omitempty"`
}

type initializeResult struct {
	Capabilities serverCapabilities `json:"capabilities"`
	ServerInfo   serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	TextDocumentSync   int                `json:"textDocumentSync"` // 1 = full
	CompletionProvider *completionOptions `json:"completionProvider,omitempty"`
	HoverProvider      bool               `json:"hoverProvider,omitempty"`
}

type completionOptions struct {
	TriggerCharacters []string `json:"triggerCharacters,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

const debounceDelay = 500 * time.Millisecond

type lspServer struct {
	mu        sync.Mutex
	docs      map[string]string          // uri -> in-memory text
	roots     map[string]string          // uri -> module root dir (cached)
	published map[string]map[string]bool // root -> file URIs with visible diagnostics
	debounce  map[string]*time.Timer     // root -> pending typed-tier publish
	mwCache   map[string]mwCacheEntry    // file path -> parsed middleware index

	out     io.Writer
	writeMu sync.Mutex
}

func newLSPServer() *lspServer {
	return &lspServer{
		docs:      map[string]string{},
		roots:     map[string]string{},
		published: map[string]map[string]bool{},
		debounce:  map[string]*time.Timer{},
		mwCache:   map[string]mwCacheEntry{},
	}
}

func (s *lspServer) send(msg any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := writeMessage(s.out, msg); err != nil {
		fmt.Fprintln(os.Stderr, "fabrik lsp: write error:", err)
	}
}

func (s *lspServer) reply(id json.RawMessage, result any) {
	body, _ := json.Marshal(result)
	s.send(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{"2.0", id, body})
}

func (s *lspServer) notify(method string, params any) {
	body, _ := json.Marshal(params)
	s.send(struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{"2.0", method, body})
}

func (s *lspServer) run(in io.Reader, out io.Writer) error {
	s.out = out
	r := bufio.NewReader(in)
	for {
		msg, err := readMessage(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if shouldExit := s.handle(msg); shouldExit {
			return nil
		}
	}
}

func (s *lspServer) handle(msg *rpcMessage) (exit bool) {
	switch msg.Method {
	case "initialize":
		s.reply(msg.ID, initializeResult{
			Capabilities: serverCapabilities{
				TextDocumentSync: 1, // Full
				CompletionProvider: &completionOptions{
					TriggerCharacters: []string{":", "=", " "},
				},
				HoverProvider: true,
			},
			ServerInfo: serverInfo{Name: "fabrik-lsp", Version: "0.2"},
		})
	case "initialized":
	case "shutdown":
		s.reply(msg.ID, nil)
	case "exit":
		return true
	case "textDocument/didOpen":
		var p didOpenParams
		_ = json.Unmarshal(msg.Params, &p)
		s.setDoc(p.TextDocument.URI, p.TextDocument.Text)
		s.onChange(p.TextDocument.URI)
	case "textDocument/didChange":
		var p didChangeParams
		_ = json.Unmarshal(msg.Params, &p)
		if len(p.ContentChanges) > 0 {
			s.setDoc(p.TextDocument.URI, p.ContentChanges[len(p.ContentChanges)-1].Text)
		}
		s.onChange(p.TextDocument.URI)
	case "textDocument/didSave":
		var p didSaveParams
		_ = json.Unmarshal(msg.Params, &p)
		if p.Text != nil {
			s.setDoc(p.TextDocument.URI, *p.Text)
		}
		s.onChange(p.TextDocument.URI)
	case "textDocument/didClose":
		var p didCloseParams
		_ = json.Unmarshal(msg.Params, &p)
		s.mu.Lock()
		delete(s.docs, p.TextDocument.URI)
		s.mu.Unlock()
	case "textDocument/completion":
		var p completionParams
		_ = json.Unmarshal(msg.Params, &p)
		s.reply(msg.ID, s.completion(p.TextDocument.URI, p.Position))
	case "textDocument/hover":
		var p hoverParams
		_ = json.Unmarshal(msg.Params, &p)
		s.reply(msg.ID, s.hover(p.TextDocument.URI, p.Position))
	default:
		if len(msg.ID) > 0 && string(msg.ID) != "null" {
			s.reply(msg.ID, nil)
		}
	}
	return false
}

// onChange publishes syntax diagnostics immediately and debounces type checks per root.
func (s *lspServer) onChange(uri string) {
	s.publishSyntactic(uri)

	root := s.rootForURI(uri)
	if root == "" {
		return
	}
	s.mu.Lock()
	if t := s.debounce[root]; t != nil {
		t.Stop()
	}
	s.debounce[root] = time.AfterFunc(debounceDelay, func() { s.publishTyped(uri) })
	s.mu.Unlock()
}

func (s *lspServer) setDoc(uri, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[uri] = text
}

func (s *lspServer) getDoc(uri string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.docs[uri]
	return t, ok
}

// rootForURI caches the nearest ancestor containing go.mod.
func (s *lspServer) rootForURI(uri string) string {
	s.mu.Lock()
	if r, ok := s.roots[uri]; ok {
		s.mu.Unlock()
		return r
	}
	s.mu.Unlock()

	path := fileFromURI(uri)
	if path == "" {
		return ""
	}
	dir := filepath.Dir(path)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			s.mu.Lock()
			s.roots[uri] = dir
			s.mu.Unlock()
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func fileFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) > 2 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

func uriFromFile(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// url.URL preserves percent-encoded client paths.
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}
