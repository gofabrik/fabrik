package middleware

import (
	"fmt"
	"strings"
)

// CSP configures Content-Security-Policy per directive (the CSP Level 3
// source-list directives). The zero value serializes to SecureHeaders'
// baseline policy. For each field: nil keeps the baseline value or
// leaves a non-baseline directive absent; a non-nil slice replaces the
// directive's complete source list; an empty non-nil slice removes the
// directive (widening it to any default-src fallback); [CSPNone] alone
// keeps the directive and denies all sources. Source lists never merge.
//
// A policy that sets script-src-elem must include the application's
// inline-script hashes there too: script-src-elem overrides script-src
// for script elements.
type CSP struct {
	DefaultSrc []string

	ScriptSrc     []string
	ScriptSrcElem []string
	ScriptSrcAttr []string

	StyleSrc     []string
	StyleSrcElem []string
	StyleSrcAttr []string

	ConnectSrc  []string
	ImgSrc      []string
	FontSrc     []string
	MediaSrc    []string
	ChildSrc    []string
	FrameSrc    []string
	WorkerSrc   []string
	ManifestSrc []string
	ObjectSrc   []string

	FormAction     []string
	BaseURI        []string
	FrameAncestors []string
}

// Quoted CSP keyword sources; hosts and schemes are written as plain
// strings ("https:", "https://*.example.com").
const (
	CSPNone           = "'none'"
	CSPSelf           = "'self'"
	CSPUnsafeInline   = "'unsafe-inline'"
	CSPUnsafeEval     = "'unsafe-eval'"
	CSPWasmUnsafeEval = "'wasm-unsafe-eval'"
	CSPStrictDynamic  = "'strict-dynamic'"
	CSPReportSample   = "'report-sample'"
	CSPUnsafeHashes   = "'unsafe-hashes'"
)

// cspBaseline mirrors the SecureHeaders default policy.
var cspBaseline = CSP{
	DefaultSrc:     []string{CSPSelf},
	FormAction:     []string{CSPSelf},
	BaseURI:        []string{CSPSelf},
	ObjectSrc:      []string{CSPNone},
	FrameAncestors: []string{CSPNone},
}

// cspDirectives fixes the serialization order; baseline directives keep
// the historical header order.
var cspDirectives = []struct {
	name  string
	field func(*CSP) []string
}{
	{"default-src", func(c *CSP) []string { return c.DefaultSrc }},
	{"script-src", func(c *CSP) []string { return c.ScriptSrc }},
	{"script-src-elem", func(c *CSP) []string { return c.ScriptSrcElem }},
	{"script-src-attr", func(c *CSP) []string { return c.ScriptSrcAttr }},
	{"style-src", func(c *CSP) []string { return c.StyleSrc }},
	{"style-src-elem", func(c *CSP) []string { return c.StyleSrcElem }},
	{"style-src-attr", func(c *CSP) []string { return c.StyleSrcAttr }},
	{"connect-src", func(c *CSP) []string { return c.ConnectSrc }},
	{"img-src", func(c *CSP) []string { return c.ImgSrc }},
	{"font-src", func(c *CSP) []string { return c.FontSrc }},
	{"media-src", func(c *CSP) []string { return c.MediaSrc }},
	{"child-src", func(c *CSP) []string { return c.ChildSrc }},
	{"frame-src", func(c *CSP) []string { return c.FrameSrc }},
	{"worker-src", func(c *CSP) []string { return c.WorkerSrc }},
	{"manifest-src", func(c *CSP) []string { return c.ManifestSrc }},
	{"form-action", func(c *CSP) []string { return c.FormAction }},
	{"base-uri", func(c *CSP) []string { return c.BaseURI }},
	{"object-src", func(c *CSP) []string { return c.ObjectSrc }},
	{"frame-ancestors", func(c *CSP) []string { return c.FrameAncestors }},
}

// bareKeywords maps every quoted-keyword constant's unquoted form to
// its constant, so a forgotten quote cannot silently become a host
// source. Derived from the constants; they cannot drift apart.
var bareKeywords = func() map[string]string {
	m := make(map[string]string)
	for _, quoted := range []string{
		CSPNone, CSPSelf, CSPUnsafeInline, CSPUnsafeEval,
		CSPWasmUnsafeEval, CSPStrictDynamic, CSPReportSample,
		CSPUnsafeHashes,
	} {
		m[strings.Trim(quoted, "'")] = quoted
	}
	return m
}()

// WithCSP replaces Content-Security-Policy with the serialized policy.
// Validation and serialization happen here, so a broken policy fails at
// construction and later mutation of the caller's slices has no effect.
func WithCSP(policy CSP) SecureHeadersOption {
	serialized := serializeCSP(&policy)
	if serialized == "" {
		panic("middleware.WithCSP: the policy removes every directive; use WithoutHeader to drop the header")
	}
	key := "Content-Security-Policy"
	return func(s *secureHeaders) { s.overrides[key] = serialized }
}

// serializeCSP validates policy and renders the header value. Nil
// fields inherit the baseline directive when one exists.
func serializeCSP(policy *CSP) string {
	var out []string
	for _, d := range cspDirectives {
		sources := d.field(policy)
		if sources == nil {
			sources = d.field(&cspBaseline)
		}
		if sources == nil || len(sources) == 0 {
			continue
		}
		validateCSPSources(d.name, sources)
		out = append(out, d.name+" "+strings.Join(sources, " "))
	}
	return strings.Join(out, "; ")
}

func validateCSPSources(directive string, sources []string) {
	for _, src := range sources {
		if src == "" {
			panic(fmt.Sprintf("middleware.WithCSP: %s contains an empty source", directive))
		}
		if quoted, bare := bareKeywords[src]; bare {
			panic(fmt.Sprintf("middleware.WithCSP: %s source %q must be quoted; use %s", directive, src, quoted))
		}
		for i := 0; i < len(src); i++ {
			b := src[i]
			if b <= 0x20 || b > 0x7e || b == ';' || b == ',' {
				panic(fmt.Sprintf("middleware.WithCSP: %s source %q contains invalid byte %#x", directive, src, b))
			}
		}
		if src == CSPNone && len(sources) > 1 {
			panic(fmt.Sprintf("middleware.WithCSP: %s mixes %s with other sources", directive, CSPNone))
		}
	}
}
