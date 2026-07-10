package web

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Response is one HTTP outcome.
type Response interface {
	Respond(w http.ResponseWriter, r *http.Request) error
}

// View renders page.Template() with page as data.
//
//	type LoginPage struct{ Error string }
//	func (LoginPage) Template() string { return "auth/login" }
//
// The returned response is immutable and safe to share.
func View(page interface{ Template() string }) Response {
	return renderResponse{name: page.Template(), data: page}
}

// Template renders a named template with data.
func Template(name string, data any) Response {
	return renderResponse{name: name, data: data}
}

// renderResponse defers to the adapter's renderer.
type renderResponse struct {
	name string
	data any
}

func (v renderResponse) Respond(w http.ResponseWriter, r *http.Request) error {
	return errors.New("web: View/Template responses render through an adapter (configure WithRenderer)")
}

// JSON responds with buffered JSON. A zero Status means 200.
type JSON struct {
	Status int // 0 means http.StatusOK
	Value  any
}

func (j JSON) Respond(w http.ResponseWriter, r *http.Request) error {
	buf, err := json.Marshal(j.Value)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if j.Status != 0 {
		w.WriteHeader(j.Status)
	}
	_, err = w.Write(buf)
	return err
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
func Text(code int, body string) Response { return direct{code, "text/plain; charset=utf-8", body} }

// HTML responds with an HTML body.
func HTML(code int, body string) Response { return direct{code, "text/html; charset=utf-8", body} }

type direct struct {
	code        int
	contentType string
	body        string
}

func (d direct) Respond(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", d.contentType)
	w.WriteHeader(d.code)
	_, err := w.Write([]byte(d.body))
	return err
}
