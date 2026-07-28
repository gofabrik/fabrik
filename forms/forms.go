// Package forms binds HTTP requests into typed structs, validates them, and
// keeps raw submitted values for form repopulation.
//
// Bind returns request parsing errors separately from field errors. [Form.Errors]
// contains conversion and validation failures to show back to the user.
//
// GET and HEAD use query values; form-encoded and multipart requests use body
// values; JSON requests decode the body. Field names come from form tags or the
// snake_case field name, and unknown submitted fields are ignored. [File]
// fields bind uploaded files from multipart requests.
//
// Validation remains HTTP-free; this package owns request binding.
package forms

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"

	"github.com/gofabrik/fabrik/validation"
)

// ErrBodyTooLarge reports that a request body exceeds the limit set by [WithMaxBytes] and carries HTTP status 413.
var ErrBodyTooLarge error = &statusErr{msg: "forms: request body too large", status: 413}

// ErrFormConsumed reports that [Bind] detected a parsed body form whose limits
// it cannot enforce; direct body reads cannot be detected.
var ErrFormConsumed = errors.New("forms: request form already parsed before Bind")

type statusErr struct {
	msg    string
	status int
	cause  error
}

func (e *statusErr) Error() string   { return e.msg }
func (e *statusErr) HTTPStatus() int { return e.status }
func (e *statusErr) Unwrap() error   { return e.cause }

func badRequest(err error) error {
	return &statusErr{msg: err.Error(), status: 400, cause: err}
}

const (
	defaultMaxBytes  = 10 << 20 // 10 MiB
	defaultMaxMemory = 10 << 20 // 10 MiB multipart in-memory threshold
)

// Form is the result of [Bind].
type Form[T any] struct {
	Data   T
	Errors validation.Errors
	raw    url.Values
}

// Valid reports whether there were no decode or validation errors.
func (f *Form[T]) Valid() bool { return f.Errors.Empty() }

// Value returns the raw submitted value for repopulating a field.
func (f *Form[T]) Value(field string) string { return f.raw.Get(field) }

// Error returns the field's error message, or "".
func (f *Form[T]) Error(field string) string { return f.Errors.Get(field) }

type config struct {
	maxBytes  int64
	maxMemory int64
}

// Option configures [Bind].
type Option func(*config)

// WithMaxBytes caps the request body (default 10 MiB). A larger body makes
// [Bind] return [ErrBodyTooLarge]; multipart framing counts toward the
// cap, so it is not a per-file size policy - validate [File.Size] for
// that.
func WithMaxBytes(n int64) Option { return func(c *config) { c.maxBytes = n } }

// WithMaxMemory sets the multipart in-memory threshold before file
// parts spill to disk (default 10 MiB).
func WithMaxMemory(n int64) Option { return func(c *config) { c.maxMemory = n } }

// Bind decodes r into a Form[T] and runs its supported validation method,
// when present.
func Bind[T any](r *http.Request, opts ...Option) (*Form[T], error) {
	typ := reflect.TypeFor[T]()
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("forms: Bind[T]: T must be a struct, got %s", typ)
	}

	cfg := config{maxBytes: defaultMaxBytes, maxMemory: defaultMaxMemory}
	for _, o := range opts {
		o(&cfg)
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(nil, r.Body, cfg.maxBytes)
	}

	form := &Form[T]{raw: url.Values{}}

	if mediaType(r) == "application/json" {
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, jsonErr(err)
			}
			// Unmarshal decodes the whole body and rejects trailing content.
			if err := json.Unmarshal(body, &form.Data); err != nil {
				return nil, jsonErr(err)
			}
			if err := rejectJSONFiles(&form.Data); err != nil {
				return nil, badRequest(err)
			}
		}
	} else {
		values, err := requestValues(r, cfg)
		if err != nil {
			return nil, err
		}
		form.raw = values
		var files map[string][]*multipart.FileHeader
		if r.MultipartForm != nil {
			files = r.MultipartForm.File
		}
		decode(values, files, &form.Errors, &form.Data)
	}

	// Add is first-wins, so decode errors take precedence.
	if v, ok := any(&form.Data).(validation.Validatable); ok {
		for field, msg := range v.Validate() {
			form.Errors.Add(field, msg)
		}
	}
	return form, nil
}

// requestValues parses non-JSON submissions.
func requestValues(r *http.Request, cfg config) (url.Values, error) {
	if mediaType(r) == "multipart/form-data" {
		// Reject prior parsing because ParseMultipartForm would silently reuse its limits.
		if r.MultipartForm != nil || r.PostForm != nil {
			return nil, ErrFormConsumed
		}
		if err := r.ParseMultipartForm(cfg.maxMemory); err != nil { // #nosec G120 -- input is length-bounded above
			return nil, parseErr(err)
		}
		if r.MultipartForm != nil {
			return url.Values(r.MultipartForm.Value), nil
		}
		return url.Values{}, nil
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if err := r.ParseForm(); err != nil {
			return nil, parseErr(err)
		}
		return r.Form, nil
	}
	// Reject prior parsing when ParseForm may have consumed a body outside Bind's limits.
	if bodyParseMethod(r.Method) && r.PostForm != nil {
		mt, err := parsedMediaType(r)
		if err != nil || mt == "application/x-www-form-urlencoded" {
			return nil, ErrFormConsumed
		}
	}
	if err := r.ParseForm(); err != nil {
		return nil, parseErr(err)
	}
	return r.PostForm, nil
}

func bodyParseMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

// parsedMediaType surfaces Content-Type parse failures; mediaType
// collapses them to "", which would skip the consumed-form guard.
func parsedMediaType(r *http.Request) (string, error) {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return "", nil
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", err
	}
	return mt, nil
}

func parseErr(err error) error {
	if tooLarge(err) {
		return ErrBodyTooLarge
	}
	return badRequest(fmt.Errorf("forms: parse request: %w", err))
}

func jsonErr(err error) error {
	if tooLarge(err) {
		return ErrBodyTooLarge
	}
	return badRequest(fmt.Errorf("forms: decode json: %w", err))
}

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func mediaType(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return mt
}
