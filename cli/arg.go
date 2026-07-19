package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// Arg defines a typed positional argument and reads its value from a [Context].
type Arg[T any] struct {
	name string
	help string

	required bool
	variadic bool
	isSlice  bool

	defaultVal T
	hasDefault bool

	parse     func(string) (T, error)
	appendOne func(prev T, s string) (T, error)
	validate  func(T) error
	completer CompleteFn
}

// StringArg declares a single-value string positional.
func StringArg(name string) *Arg[string] {
	return &Arg[string]{
		name:  name,
		parse: func(s string) (string, error) { return s, nil },
	}
}

// IntArg declares a single-value int positional.
func IntArg(name string) *Arg[int] {
	return &Arg[int]{
		name: name,
		parse: func(s string) (int, error) {
			v, err := strconv.ParseInt(s, 10, 64)
			return int(v), err
		},
	}
}

// StringSliceArg declares a string slice positional for use with [Arg.Variadic].
func StringSliceArg(name string) *Arg[[]string] {
	return &Arg[[]string]{
		name:    name,
		isSlice: true,
		parse: func(s string) ([]string, error) {
			return []string{s}, nil
		},
		appendOne: func(prev []string, s string) ([]string, error) {
			return append(prev, s), nil
		},
	}
}

// CustomArg declares a positional for a user-defined type.
func CustomArg[T any](name string, parse func(string) (T, error)) *Arg[T] {
	return &Arg[T]{
		name:  name,
		parse: parse,
	}
}

func (a *Arg[T]) Help(s string) *Arg[T] { a.help = s; return a }
func (a *Arg[T]) Required() *Arg[T]     { a.required = true; return a }
func (a *Arg[T]) Default(v T) *Arg[T]   { a.defaultVal = v; a.hasDefault = true; return a }
func (a *Arg[T]) Validate(fn func(T) error) *Arg[T] {
	a.validate = fn
	return a
}
func (a *Arg[T]) Complete(fn CompleteFn) *Arg[T] { a.completer = fn; return a }

// OneOf restricts accepted values using [fmt.Sprint] and registers matching completion candidates.
func (a *Arg[T]) OneOf(allowed ...T) *Arg[T] {
	if len(allowed) == 0 {
		return a
	}
	keys := make([]string, len(allowed))
	set := make(map[string]struct{}, len(allowed))
	for i, v := range allowed {
		keys[i] = fmt.Sprint(v)
		set[keys[i]] = struct{}{}
	}
	a.validate = func(v T) error {
		if _, ok := set[fmt.Sprint(v)]; !ok {
			return fmt.Errorf("value %v is not one of [%s]", v, strings.Join(keys, ", "))
		}
		return nil
	}
	a.completer = StaticValues(keys...)
	return a
}

// Variadic makes the final slice argument collect all remaining tokens; other placements fail declaration validation during parsing.
func (a *Arg[T]) Variadic() *Arg[T] {
	a.variadic = true
	return a
}

func (a *Arg[T]) Name() string { return a.name }

// Get returns the parsed value, or the default, or the zero value.
func (a *Arg[T]) Get(ctx Context) T {
	v := valuesFromCtx(ctx)
	if raw, ok := v[a]; ok {
		return raw.(T)
	}
	if a.hasDefault {
		return a.defaultVal
	}
	var zero T
	return zero
}

// Lookup distinguishes an explicit value from a default or zero-value fallback.
func (a *Arg[T]) Lookup(ctx Context) (T, bool) {
	v := valuesFromCtx(ctx)
	if raw, ok := v[a]; ok {
		return raw.(T), true
	}
	var zero T
	return zero, false
}

func (a *Arg[T]) argName() string   { return a.name }
func (a *Arg[T]) argHelp() string   { return a.help }
func (a *Arg[T]) argRequired() bool { return a.required }
func (a *Arg[T]) argVariadic() bool { return a.variadic }
func (a *Arg[T]) argIsSlice() bool  { return a.isSlice }

func (a *Arg[T]) argApplyString(v values, s string) error {
	if a.isSlice {
		if _, ok := v[a]; !ok {
			val, err := a.parse(s)
			if err != nil {
				return a.parseErr(s, err)
			}
			v[a] = val
			return nil
		}
		prev := v[a].(T)
		next, err := a.appendOne(prev, s)
		if err != nil {
			return a.parseErr(s, err)
		}
		v[a] = next
		return nil
	}
	val, err := a.parse(s)
	if err != nil {
		return a.parseErr(s, err)
	}
	v[a] = val
	return nil
}

func (a *Arg[T]) argPresent(v values) bool {
	_, ok := v[a]
	return ok
}

func (a *Arg[T]) argValidate(v values) error {
	if a.validate == nil {
		return nil
	}
	var val T
	if raw, ok := v[a]; ok {
		val = raw.(T)
	} else if a.hasDefault {
		val = a.defaultVal
	} else {
		return nil
	}
	if err := a.validate(val); err != nil {
		return fmt.Errorf("argument %s: %v: %w", a.name, err, ErrValidation)
	}
	return nil
}

func (a *Arg[T]) parseErr(s string, err error) error {
	return fmt.Errorf("argument %s=%q: %v: %w", a.name, s, err, ErrUsage)
}

func (a *Arg[T]) argCompleter() CompleteFn { return a.completer }
