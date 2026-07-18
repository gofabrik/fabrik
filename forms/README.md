# forms

HTTP request binding for Go. Decode a request into a typed struct, validate it with [`validation`](../validation), and re-render failed forms with field errors and raw input intact.

## Features

| Feature | What it gives you |
|---|---|
| Typed binding | `Bind[T]` decodes form, multipart, query, or JSON input into `T`. |
| Field-keyed errors | Conversion and validation errors keyed by field, for one message per input. |
| Repopulation | `Value(field)` returns the **raw** submission, so a bad `"abc"` in an int field redraws as `"abc"`, not `0`. |
| Two-level errors | An `error` for unparseable requests (400/413); `Form.Errors` for user-input problems (re-render). |
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
    if err != nil {                  // unparseable request
        http.Error(w, "bad request", http.StatusBadRequest); return
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
// err != nil means an unparseable request, return 400 or 413.
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

## Status

Reference code. File uploads are read via `r.FormFile` after `Bind`. Nested structs and `time.Time` are not bound yet.
