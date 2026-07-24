# forms

HTTP request binding for Go. Decode a request into a typed struct, validate it with [`validation`](../validation), and re-render failed forms with field errors and raw input intact.

## Features

| Feature | What it gives you |
|---|---|
| Typed binding | `Bind[T]` decodes form, multipart, query, or JSON input into `T`. |
| Field-keyed errors | Conversion and validation errors keyed by field, for one message per input. |
| Repopulation | `Value(field)` returns the **raw** submission, so a bad `"abc"` in an int field redraws as `"abc"`, not `0`. |
| Two-level errors | An `error` for request-level failures (malformed 400, oversized 413, or the status-less `ErrFormConsumed` ordering bug); `Form.Errors` for user-input problems (re-render). |
| Validation built in | If `T` implements `validation.Validatable`, `Bind` runs it and merges the result. |

## Install

```bash
go get github.com/gofabrik/fabrik/forms
```

## Usage

```go
type LoginInput struct {
    Email    string
    Password string
}
func (in LoginInput) Validate() validation.Errors {
    return validation.Check(
        validation.Field("email",    in.Email,    validation.Required(), validation.Email()),
        validation.Field("password", in.Password, validation.Required(), validation.MinLen(8)),
    )
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
    form, err := forms.Bind[LoginInput](r)
    if err != nil {                  // request-level failure
        switch {
        case errors.Is(err, forms.ErrBodyTooLarge):
            http.Error(w, "too large", http.StatusRequestEntityTooLarge)
        case errors.Is(err, forms.ErrFormConsumed):
            http.Error(w, "server error", http.StatusInternalServerError)
        default:
            http.Error(w, "bad request", http.StatusBadRequest)
        }
        return
    }
    if !form.Valid() {               // field errors, re-render
        render(w, "login", page{Form: form}); return
    }
    authenticate(form.Data.Email, form.Data.Password)
}
```

In the template:

```html
<input name="email" value="{{ .Form.Value "email" }}">
{{ with .Form.Error "email" }}<span class="err">{{ . }}</span>{{ end }}
```

## Binding rules

- **Sources**: `GET`/`HEAD` use the query string; form-encoded and multipart `POST` use the body; `application/json` uses the body.
- **Field names**: a `form:"name"` tag, else snake_case of the field. The mapping matches the `query` package's column mapping, so one struct can be used for binding and persistence. `form:"-"` skips a field; unknown submitted fields such as `csrf_token` are ignored.
- **Types**: `string`, int/uint/float kinds, `bool`, `[]string`, and pointers to scalars. A `bool` binds `on`/`true`/`1`/`yes` as true, and a missing checkbox as false. A pointer field is nil when input is absent or blank, and set otherwise. Unsupported kinds such as nested structs, maps, and `time.Time` stay zero in v1.
- **Conversion errors** (e.g. `"abc"` into an `int`) become field errors and the raw value is kept for repopulation; the typed field stays zero.

## Errors

```go
form, err := forms.Bind[T](r)
// err != nil is a request-level failure: malformed input (400),
// an oversized body (ErrBodyTooLarge, 413), or ErrFormConsumed - a
// handler-ordering bug that deserves a 500, not a client error.
// !form.Valid() means form.Errors has field errors, re-render.
```

`form.Errors` is a `validation.Errors`. Decode (type) errors take precedence over a validation error on the same field.

## Options

```go
forms.WithMaxBytes(1 << 20)   // cap the body, default 10 MiB
forms.WithMaxMemory(8 << 20)  // multipart in-memory threshold (default 10 MiB)
```

A body over the configured limit makes `Bind` return `ErrBodyTooLarge` on every path (urlencoded, multipart, and JSON).

## JSON

JSON decodes the whole body via `encoding/json` using `json:` tags, then runs validation. Type mismatches return a single `error`, not a per-field error. JSON binding does not keep raw values for repopulation.

## File uploads

A `forms.File` (or `[]forms.File`) field binds multipart uploads: `Open` returns the content as an `io.ReadSeekCloser` (`ErrNoFile` when absent), `Size` is parser-counted, and the client filename and content type are untrusted metadata. Files must be consumed before the handler returns. Non-null JSON values for file fields fail binding.

`Bind` must run before `net/http` parses a body form so it can enforce configured limits; detected prior parsing returns `ErrFormConsumed`. Unlike the other request-level errors (oversize 413, malformed 400), `ErrFormConsumed` carries no HTTP status: it reports a handler-ordering mistake, not client input, and status-mapping adapters treat it as a server error.

## Status

Reference code. Nested structs and `time.Time` are not bound yet.
