# validation

Code-based input validation for Go. Types declare rules in `Validate` methods and return field-keyed errors.

## Features

| Feature | What it gives you |
|---|---|
| Code-based, type-safe | Rules are `Rule[T]` values checked by the compiler, with no `validate:"..."` tag DSL. |
| Field-keyed errors | `Errors` is `map[field]message`, so a UI shows one message per input. |
| Type owns its rules | A `Validate() Errors` method works everywhere; no duplication across form/API. |
| No typed-nil trap | `Check` returns the concrete `Errors`; test with `.Empty()`. |
| stdlib only | `regexp`, `cmp`, `net/mail`, `net/url`. |

## Install

```bash
go get github.com/gofabrik/fabrik/validation
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

errs := input.Validate()
if !errs.Empty() {
    // errs.Get("email") == "must be a valid email address"
}
```

Rules run in order and the **first failure wins** per field, so list the most fundamental rule (usually `Required`) first.

## Rules

Parameterized rules infer their type from the argument, so `Field` rarely needs explicit type parameters.

Every rule is a function call, so the API is uniform:

```go
// Rule[string]
validation.Required()                // non-blank (empty or whitespace fails)
validation.Email()
validation.URL()
validation.MinLen(8)                 // rune count
validation.MaxLen(100)
validation.Pattern(re)               // *regexp.Regexp

// inferred from the argument
validation.Min(18)                   // Rule[int] (any cmp.Ordered)
validation.Max(65535)
validation.In("http", "tcp")         // Rule[T comparable]
validation.By(func(s string) error { ... }) // custom / cross-field
```

**Empty-string convention:** format and length rules (`Email`, `URL`, `MinLen`, `MaxLen`, `Pattern`) treat `""` as valid. Add `Required` to enforce presence. Numeric and set rules (`Min`, `Max`, `In`) treat zero values as real values, and `NaN` fails `Min`/`Max`.

`Email` is a practical check, not full RFC 5322: it rejects display-name/comment forms (`Bob <a@b.com>`) and dotless domains (`a@b`, `x@localhost`), which is what a form's email field expects.

**Numeric inference:** `Min`/`Max` infer their type from the argument. Match non-`int` fields explicitly, for example `Min(int32(1))` for an `int32` field or `Min(1.0)` for a `float64`.

## Errors

```go
type Errors map[string]string

func (Errors) Empty() bool
func (Errors) Has(field string) bool
func (Errors) Get(field string) string
func (*Errors) Add(field, msg string)   // first wins; "" is form-level
func (Errors) Error() string            // "email: ...; password: ..." (sorted)
```

The empty key `""` is a form-level error, such as "email or password is wrong". `Errors` implements `error`; use `Empty()` for validity checks instead of comparing an `error` interface to nil.

## Status

Reference code. Flat structs and fixed English messages only. The `forms` package builds on this for HTTP request binding and repopulation.
