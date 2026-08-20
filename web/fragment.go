package web

import (
	"fmt"
	"net/http"
	"strings"
)

// RegionRenderer is a [Renderer] whose pages declare which of their pieces
// may be asked for by name. Only [TemplateResponse.Fragment] needs one.
//
// Region resolves a page's public region name to the template that renders
// it, reporting whether the page declares one. It is what keeps a name that
// came from the client from becoming a template name.
type RegionRenderer interface {
	Renderer
	Region(page, name string) (define string, ok bool)
}

// ErrNoRegionRenderer reports a Fragment response on an adapter whose renderer
// cannot resolve regions.
var ErrNoRegionRenderer = fmt.Errorf("web: Fragment responses need a renderer that resolves regions (see RegionRenderer)")

// ErrNoTarget reports an htmx request that asked for a region without saying
// which. It carries HTTP status 400.
//
// htmx sends HX-Target as the target element's id and omits the header when
// the target has none, so this is what an unidentified target arrives as. A
// page cannot answer it: sending the whole document would swap a page into
// whatever small element was aimed at. Give the target an id matching a
// declared region, or answer with [TemplateResponse.Block] instead.
var ErrNoTarget error = &statusErr{
	msg:     "web: htmx request has no HX-Target to resolve a region from",
	status:  http.StatusBadRequest,
	headers: []headerPair{{"Cache-Control", "no-store"}},
}

// ErrUnknownRegion reports a target naming no region the page declares. It
// carries HTTP status 404.
var ErrUnknownRegion error = &statusErr{
	msg:     "web: no such region",
	status:  http.StatusNotFound,
	headers: []headerPair{{"Cache-Control", "no-store"}},
}

type statusErr struct {
	msg     string
	status  int
	cause   error
	headers []headerPair
}

func (e *statusErr) Error() string   { return e.msg }
func (e *statusErr) HTTPStatus() int { return e.status }
func (e *statusErr) Unwrap() error   { return e.cause }

// ResponseHeaders is what an adapter applies to the error's response after it
// has put the header map back, so a rejection can still say something about
// itself that must not be lost with the headers of the response that failed.
func (e *statusErr) ResponseHeaders() []headerPair { return e.headers }

// swapsAFragment reports whether this is htmx replacing part of a page rather
// than asking for the page itself.
//
// A boosted navigation and a history restoration both carry HX-Request and
// both want the whole document, so they are not fragment swaps however they
// are targeted.
func swapsAFragment(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" &&
		r.Header.Get("HX-Boosted") != "true" &&
		r.Header.Get("HX-History-Restore-Request") != "true"
}

// renderFragment answers with one declared region for an htmx swap, and with
// the whole page for everything else.
func (a *Adapter) renderFragment(w http.ResponseWriter, r *http.Request, v TemplateResponse) error {
	regions, ok := a.renderer.(RegionRenderer)
	if !ok {
		return ErrNoRegionRenderer
	}

	// One URL now answers with either a page or a piece of one, so whatever
	// caches it has to key on what chose between them. This goes in with the
	// response's own headers rather than onto the writer, because those are
	// applied with Set: a response carrying a Vary of its own would otherwise
	// replace these rather than join them.
	v.headers = mergeVary(v.headers, w.Header(), varyOnSelection...)

	if !swapsAFragment(r) {
		return a.render(w, r, v.name, a.blockFor(v.block), v.status, v.headers, v.data)
	}

	target := r.Header.Get("HX-Target")
	if target == "" {
		return ErrNoTarget
	}
	define, ok := regions.Region(v.name, target)
	if !ok {
		return &statusErr{
			msg:    fmt.Sprintf("web: page %q declares no region %q", v.name, target),
			status: http.StatusNotFound,
			cause:  ErrUnknownRegion,
			// The error leaves through the error handler, which restores the
			// header map first, so this response cannot carry the Vary above.
			// Not storing it at all is what keeps a cache from answering an
			// ordinary request with it later.
			headers: []headerPair{{"Cache-Control", "no-store"}},
		}
	}
	return a.render(w, r, v.name, define, v.status, v.headers, v.data)
}

// varyOnSelection are the request fields a fragment response's body depends on.
var varyOnSelection = []string{"HX-Request", "HX-Boosted", "HX-History-Restore-Request", "HX-Target"}

// mergeVary returns headers carrying one Vary that joins names with whatever
// Vary the response and the writer already hold.
//
// The response's own Vary is replaced rather than appended to, because these
// are applied with Set and two pairs would mean the last one wins.
func mergeVary(headers []headerPair, existing http.Header, names ...string) []headerPair {
	var lines []string
	lines = append(lines, existing.Values("Vary")...)

	out := make([]headerPair, 0, len(headers)+1)
	for _, h := range headers {
		if http.CanonicalHeaderKey(h.key) == "Vary" {
			lines = append(lines, h.value)
			continue
		}
		out = append(out, h)
	}

	value := joinVary(append(lines, names...))
	if value == "" {
		return headers
	}
	return append(out, headerPair{"Vary", value})
}

// joinVary folds Vary lines and field names into one value, without repeating
// a field. A "*" absorbs the rest: the grammar is a list of names or that,
// never both, and it already says the response cannot be reused.
func joinVary(lines []string) string {
	seen := map[string]bool{}
	var values []string
	for _, line := range lines {
		for _, name := range strings.Split(line, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if name == "*" {
				return "*"
			}
			if !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				values = append(values, name)
			}
		}
	}
	return strings.Join(values, ", ")
}
