package web

import (
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"errors"
	"net/http"
	"slices"
)

// Response is one HTTP outcome.
type Response interface {
	Respond(w http.ResponseWriter, r *http.Request) error
}

// Template renders a named HTML template with data.
//
// The zero status is 200 and the zero block is the adapter's [WithBlock].
// Both are set by chaining, and each call returns a new value rather than
// changing the one it was called on:
//
//	web.Template("article/form", data).Status(http.StatusUnprocessableEntity)
//	web.Template("todo/list", todo).Block("row")
func Template(name string, data any) TemplateResponse {
	return TemplateResponse{name: name, data: data}
}

// TemplateResponse is a template render. Chaining returns a copy, so a value
// shared between handlers is safe from what any of them adds to it.
//
// It is not comparable: it carries a header slice. [Status] and [Redirect]
// are the responses that compare directly.
type TemplateResponse struct {
	name    string
	block   string
	data    any
	status  int
	headers []headerPair
}

// Status returns a copy responding with code instead of 200.
func (v TemplateResponse) Status(code int) TemplateResponse {
	v.status = code
	return v
}

// Header returns a copy carrying one more header. A header set here wins over
// one recorded on the request.
func (v TemplateResponse) Header(key, value string) TemplateResponse {
	v.headers = withHeader(v.headers, key, value)
	return v
}

// Block returns a copy rendering the named block of the template instead of
// the adapter's default: the fragment an htmx swap replaces rather than the
// whole document.
func (v TemplateResponse) Block(name string) TemplateResponse {
	v.block = name
	return v
}

// ErrTemplateWithoutAdapter reports a Template response responding outside an adapter.
var ErrTemplateWithoutAdapter = errors.New("web: Template responses render through an adapter (configure WithRenderer)")

func (v TemplateResponse) Respond(w http.ResponseWriter, r *http.Request) error {
	return ErrTemplateWithoutAdapter
}

// JSON responds with buffered JSON, so an encoding failure reaches the error
// handler with nothing written.
//
//	web.JSON(items)
//	web.JSON(item).Status(http.StatusCreated)
//	web.JSON(counts).Header("Cache-Control", "no-store")
func JSON(value any) JSONResponse {
	return JSONResponse{value: value}
}

// JSONResponse is a JSON body with the status and headers it carries.
type JSONResponse struct {
	value   any
	status  int
	headers []headerPair
}

// Status returns a copy responding with code instead of 200.
func (v JSONResponse) Status(code int) JSONResponse {
	v.status = code
	return v
}

// Header returns a copy carrying one more header. A header set here wins over
// one recorded on the request.
func (v JSONResponse) Header(key, value string) JSONResponse {
	v.headers = withHeader(v.headers, key, value)
	return v
}

func (v JSONResponse) Respond(w http.ResponseWriter, r *http.Request) error {
	buf, err := json.Marshal(v.value, jsonv1.DefaultOptionsV1())
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	applyHeaders(w, v.headers)
	if v.status != 0 {
		w.WriteHeader(v.status)
	}
	_, err = w.Write(buf)
	return err
}

type headerPair struct{ key, value string }

// withHeader appends without letting a copy share the original's array, so
// chaining from one response never changes another.
func withHeader(headers []headerPair, key, value string) []headerPair {
	return append(slices.Clip(headers), headerPair{key, value})
}

func applyHeaders(w http.ResponseWriter, headers []headerPair) {
	for _, h := range headers {
		w.Header().Set(h.key, h.value)
	}
}

// Redirect responds with 303 See Other and a Location header.
//
// The URL is sent verbatim; use absolute paths.
type Redirect string

func (d Redirect) Respond(w http.ResponseWriter, r *http.Request) error {
	return redirect(w, string(d), http.StatusSeeOther)
}

// RedirectPermanent responds with 308 Permanent Redirect and a Location header.
type RedirectPermanent string

func (d RedirectPermanent) Respond(w http.ResponseWriter, r *http.Request) error {
	return redirect(w, string(d), http.StatusPermanentRedirect)
}

func redirect(w http.ResponseWriter, url string, code int) error {
	w.Header().Set("Location", url)
	w.WriteHeader(code)
	return nil
}

// Status responds with the code and no body. Comparable.
type Status int

func (s Status) Respond(w http.ResponseWriter, r *http.Request) error {
	w.WriteHeader(int(s))
	return nil
}

// Text responds with a plain-text body.
func Text(code int, body string) DirectResponse {
	return DirectResponse{code: code, contentType: "text/plain; charset=utf-8", body: body}
}

// HTML responds with an HTML body.
func HTML(code int, body string) DirectResponse {
	return DirectResponse{code: code, contentType: "text/html; charset=utf-8", body: body}
}

// DirectResponse is a small body written as given, from [Text] or [HTML].
type DirectResponse struct {
	code        int
	contentType string
	body        string
	headers     []headerPair
}

// Header returns a copy carrying one more header.
func (v DirectResponse) Header(key, value string) DirectResponse {
	v.headers = withHeader(v.headers, key, value)
	return v
}

func (v DirectResponse) Respond(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", v.contentType)
	applyHeaders(w, v.headers)
	w.WriteHeader(v.code)
	_, err := w.Write([]byte(v.body))
	return err
}
