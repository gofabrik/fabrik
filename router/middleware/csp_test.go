package middleware

import (
	"net/http"
	"strings"
	"testing"
)

func cspHeader(t *testing.T, opts ...SecureHeadersOption) string {
	t.Helper()
	return serve(SecureHeaders(opts...), false, nil).Get("Content-Security-Policy")
}

func TestCSPZeroValueIsTheBaseline(t *testing.T) {
	if got, want := cspHeader(t, WithCSP(CSP{})), cspHeader(t); got != want {
		t.Fatalf("CSP{} = %q, baseline = %q", got, want)
	}
}

func TestCSPFieldReplacesOnlyItsDirective(t *testing.T) {
	got := cspHeader(t, WithCSP(CSP{ScriptSrc: []string{CSPSelf, "'sha256-abc'"}}))
	want := "default-src 'self'; script-src 'self' 'sha256-abc'; form-action 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCSPEveryFieldMapsToItsDirective(t *testing.T) {
	cases := map[string]CSP{
		"default-src 'none'":     {DefaultSrc: []string{CSPNone}},
		"script-src-elem 'self'": {ScriptSrcElem: []string{CSPSelf}},
		"script-src-attr 'none'": {ScriptSrcAttr: []string{CSPNone}},
		"style-src 'self'":       {StyleSrc: []string{CSPSelf}},
		"style-src-elem 'self'":  {StyleSrcElem: []string{CSPSelf}},
		"style-src-attr 'none'":  {StyleSrcAttr: []string{CSPNone}},
		"connect-src 'self'":     {ConnectSrc: []string{CSPSelf}},
		"img-src https:":         {ImgSrc: []string{"https:"}},
		"font-src 'self'":        {FontSrc: []string{CSPSelf}},
		"media-src 'none'":       {MediaSrc: []string{CSPNone}},
		"child-src 'self'":       {ChildSrc: []string{CSPSelf}},
		"frame-src 'none'":       {FrameSrc: []string{CSPNone}},
		"worker-src 'self'":      {WorkerSrc: []string{CSPSelf}},
		"manifest-src 'self'":    {ManifestSrc: []string{CSPSelf}},
	}
	for want, policy := range cases {
		if got := cspHeader(t, WithCSP(policy)); !strings.Contains(got, want) {
			t.Fatalf("policy %+v missing %q: %q", policy, want, got)
		}
	}
}

func TestCSPEmptyRemovesVersusNoneDenies(t *testing.T) {
	removed := cspHeader(t, WithCSP(CSP{ObjectSrc: []string{}}))
	if strings.Contains(removed, "object-src") {
		t.Fatalf("empty slice did not remove the directive: %q", removed)
	}
	denied := cspHeader(t, WithCSP(CSP{ObjectSrc: []string{CSPNone}}))
	if !strings.Contains(denied, "object-src 'none'") {
		t.Fatalf("CSPNone did not deny: %q", denied)
	}
}

func TestCSPSnapshotsSlices(t *testing.T) {
	sources := []string{CSPSelf}
	mw := SecureHeaders(WithCSP(CSP{ScriptSrc: sources}))
	sources[0] = "'sha256-mutated'"
	got := serve(mw, false, nil).Get("Content-Security-Policy")
	if strings.Contains(got, "mutated") {
		t.Fatalf("later slice mutation reached the middleware: %q", got)
	}
}

func TestCSPTypedAndRawLastWins(t *testing.T) {
	raw := "default-src 'none'"
	if got := cspHeader(t, WithCSP(CSP{ScriptSrc: []string{CSPSelf}}), WithContentSecurityPolicy(raw)); got != raw {
		t.Fatalf("raw after typed should win: %q", got)
	}
	got := cspHeader(t, WithContentSecurityPolicy(raw), WithCSP(CSP{ScriptSrc: []string{CSPSelf}}))
	if got == raw || !strings.Contains(got, "script-src 'self'") {
		t.Fatalf("typed after raw should win: %q", got)
	}
}

func TestCSPWithoutHeaderDominates(t *testing.T) {
	for _, mw := range []func(http.Handler) http.Handler{
		SecureHeaders(WithoutHeader("Content-Security-Policy"), WithCSP(CSP{ScriptSrc: []string{CSPSelf}})),
		SecureHeaders(WithCSP(CSP{ScriptSrc: []string{CSPSelf}}), WithoutHeader("Content-Security-Policy")),
	} {
		if got := serve(mw, false, nil).Get("Content-Security-Policy"); got != "" {
			t.Fatalf("omission lost to WithCSP: %q", got)
		}
	}
}

func TestCSPValidationPanics(t *testing.T) {
	cases := map[string]CSP{
		"empty token":         {ScriptSrc: []string{""}},
		"semicolon":           {ScriptSrc: []string{"a;b"}},
		"internal whitespace": {ScriptSrc: []string{"self https://x"}},
		"bare self":           {ScriptSrc: []string{"self"}},
		"bare none":           {ObjectSrc: []string{"none"}},
		"bare unsafe-inline":  {StyleSrc: []string{"unsafe-inline"}},
		"bare unsafe-eval":    {ScriptSrc: []string{"unsafe-eval"}},
		"bare wasm":           {ScriptSrc: []string{"wasm-unsafe-eval"}},
		"bare strict-dynamic": {ScriptSrc: []string{"strict-dynamic"}},
		"bare report-sample":  {ScriptSrc: []string{"report-sample"}},
		"bare unsafe-hashes":  {ScriptSrc: []string{"unsafe-hashes"}},
		"none mixed":          {ScriptSrc: []string{CSPNone, CSPSelf}},
		"control byte":        {ScriptSrc: []string{"a\x01b"}},
		"fully empty policy":  {DefaultSrc: []string{}, FormAction: []string{}, BaseURI: []string{}, ObjectSrc: []string{}, FrameAncestors: []string{}},
	}
	for name, policy := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: no panic", name)
				}
			}()
			WithCSP(policy)
		}()
	}
	// Valid host, scheme, wildcard, nonce, and hash tokens pass.
	cspHeader(t, WithCSP(CSP{ScriptSrc: []string{CSPSelf, "https:", "https://*.example.com", "'nonce-abc'", "'sha256-xyz'"}}))
}
