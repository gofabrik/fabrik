// Package validation provides code-based input validation with field-keyed errors.
//
// Types declare rules in Validate methods using [Field] and [Check], keeping
// validation independent of the input source.
//
// [Check] returns concrete [Errors], not the error interface. Use [Errors.Empty]
// for validity checks to avoid typed-nil surprises.
//
// Format and length rules treat "" as valid; add [Required] to enforce
// presence. Numeric and set rules treat zero values as real values.
package validation

import (
	"sort"
	"strings"
)

// Rule checks a value and returns a user-facing error when invalid.
// A plain func(T) error satisfies Rule.
type Rule[T any] func(value T) error

// Errors maps field names to their first error message. Nil means valid, and
// key "" is a form-level error. Errors implements error.
type Errors map[string]string

// Empty reports whether there are no errors.
func (e Errors) Empty() bool { return len(e) == 0 }

// Has reports whether field has an error.
func (e Errors) Has(field string) bool { _, ok := e[field]; return ok }

// Get returns field's error message, or "" if it has none.
func (e Errors) Get(field string) string { return e[field] }

// Add records the first message for field, allocating the map if needed.
func (e *Errors) Add(field, msg string) {
	if *e == nil {
		*e = Errors{}
	}
	if _, ok := (*e)[field]; !ok {
		(*e)[field] = msg
	}
}

// Error joins errors in sorted field order. Key "" omits the field prefix.
func (e Errors) Error() string {
	if len(e) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("; ")
		}
		if k != "" {
			b.WriteString(k)
			b.WriteString(": ")
		}
		b.WriteString(e[k])
	}
	return b.String()
}

// Validatable is implemented by values that validate themselves.
type Validatable interface {
	Validate() Errors
}

// Constraint is one field validation built by [Field].
type Constraint interface {
	check() (field, message string)
}

type constraint[T any] struct {
	field string
	value T
	rules []Rule[T]
}

func (c constraint[T]) check() (string, string) {
	for _, r := range c.rules {
		if r == nil {
			continue
		}
		if err := r(c.value); err != nil {
			return c.field, err.Error()
		}
	}
	return c.field, ""
}

// Field binds a field name, value, and rules. Rules run in order and the first
// failure wins, so put [Required] before format or length rules.
func Field[T any](name string, value T, rules ...Rule[T]) Constraint {
	return constraint[T]{field: name, value: value, rules: rules}
}

// Check runs every constraint and returns nil when all pass.
func Check(constraints ...Constraint) Errors {
	var errs Errors
	for _, c := range constraints {
		field, msg := c.check()
		if msg == "" {
			continue
		}
		if errs == nil {
			errs = Errors{}
		}
		if _, exists := errs[field]; !exists {
			errs[field] = msg
		}
	}
	return errs
}
