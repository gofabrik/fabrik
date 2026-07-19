package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Flag defines a typed flag and reads its value from a [Context].
type Flag[T any] struct {
	name        string
	short       rune
	help        string
	env         string
	placeholder string
	group       string

	required bool
	hidden   bool
	isBool   bool
	isSlice  bool

	defaultVal T
	hasDefault bool
	defaultStr string // human-readable for help; computed in Default

	// Slice parsers return the first element and appendOne handles repeats.
	parse     func(string) (T, error)
	appendOne func(prev T, s string) (T, error)

	validate  func(T) error
	completer CompleteFn
}

// StringFlag declares a string-valued flag.
func StringFlag(name string) *Flag[string] {
	return &Flag[string]{
		name:  name,
		parse: func(s string) (string, error) { return s, nil },
	}
}

// IntFlag declares an int-valued flag.
func IntFlag(name string) *Flag[int] {
	return &Flag[int]{
		name: name,
		parse: func(s string) (int, error) {
			v, err := strconv.ParseInt(s, 10, 64)
			return int(v), err
		},
	}
}

// Int64Flag declares an int64-valued flag.
func Int64Flag(name string) *Flag[int64] {
	return &Flag[int64]{
		name:  name,
		parse: func(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) },
	}
}

// FloatFlag declares a float64-valued flag.
func FloatFlag(name string) *Flag[float64] {
	return &Flag[float64]{
		name:  name,
		parse: func(s string) (float64, error) { return strconv.ParseFloat(s, 64) },
	}
}

// BoolFlag declares a boolean flag that accepts a bare name or a =true/=false suffix.
func BoolFlag(name string) *Flag[bool] {
	return &Flag[bool]{
		name:   name,
		isBool: true,
		parse:  func(s string) (bool, error) { return strconv.ParseBool(s) },
	}
}

// DurationFlag declares a [time.Duration] flag parsed by [time.ParseDuration].
func DurationFlag(name string) *Flag[time.Duration] {
	return &Flag[time.Duration]{
		name:  name,
		parse: func(s string) (time.Duration, error) { return time.ParseDuration(s) },
	}
}

// TimeFlag declares a [time.Time] flag, parsed as RFC3339.
func TimeFlag(name string) *Flag[time.Time] {
	return &Flag[time.Time]{
		name:  name,
		parse: func(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) },
	}
}

// StringSliceFlag declares a repeatable flag that preserves value order.
func StringSliceFlag(name string) *Flag[[]string] {
	return &Flag[[]string]{
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

// IntSliceFlag declares a repeatable int flag.
func IntSliceFlag(name string) *Flag[[]int] {
	parseOne := func(s string) (int, error) {
		v, err := strconv.ParseInt(s, 10, 64)
		return int(v), err
	}
	return &Flag[[]int]{
		name:    name,
		isSlice: true,
		parse: func(s string) ([]int, error) {
			v, err := parseOne(s)
			if err != nil {
				return nil, err
			}
			return []int{v}, nil
		},
		appendOne: func(prev []int, s string) ([]int, error) {
			v, err := parseOne(s)
			if err != nil {
				return prev, err
			}
			return append(prev, v), nil
		},
	}
}

// CustomFlag declares a flag parsed from its raw token by parse.
func CustomFlag[T any](name string, parse func(string) (T, error)) *Flag[T] {
	return &Flag[T]{
		name:  name,
		parse: parse,
	}
}

// Default sets the value Get returns when the flag is not present.
func (f *Flag[T]) Default(v T) *Flag[T] {
	f.defaultVal = v
	f.hasDefault = true
	f.defaultStr = fmt.Sprint(v)
	return f
}

// Env uses an environment variable when the flag is absent from the command line.
func (f *Flag[T]) Env(name string) *Flag[T] {
	f.env = name
	return f
}

// Short sets a single-character alias usable as -x.
func (f *Flag[T]) Short(r rune) *Flag[T] {
	f.short = r
	return f
}

// Help sets the one-line description shown in --help.
func (f *Flag[T]) Help(s string) *Flag[T] {
	f.help = s
	return f
}

// Required rejects an unset flag unless a default or environment value resolves.
func (f *Flag[T]) Required() *Flag[T] {
	f.required = true
	return f
}

// Validate registers a validator run after parse succeeds.
func (f *Flag[T]) Validate(fn func(T) error) *Flag[T] {
	f.validate = fn
	return f
}

// Hidden omits the flag from --help while leaving it parseable.
func (f *Flag[T]) Hidden() *Flag[T] {
	f.hidden = true
	return f
}

// Group renders flags with the same group under one help heading.
func (f *Flag[T]) Group(name string) *Flag[T] {
	f.group = name
	return f
}

// Placeholder sets the --help metavariable, which defaults to the upper-cased flag name.
func (f *Flag[T]) Placeholder(s string) *Flag[T] {
	f.placeholder = s
	return f
}

// Complete attaches a shell-completion strategy. See [CompleteFn].
func (f *Flag[T]) Complete(fn CompleteFn) *Flag[T] {
	f.completer = fn
	return f
}

// Name returns the long-form flag name without leading dashes.
func (f *Flag[T]) Name() string { return f.name }

// Get returns the parsed value, configured default, or zero value of T.
func (f *Flag[T]) Get(ctx Context) T {
	v := valuesFromCtx(ctx)
	if raw, ok := v[f]; ok {
		return raw.(T)
	}
	if f.hasDefault {
		return f.defaultVal
	}
	var zero T
	return zero
}

// Lookup returns the value and whether parsing or environment resolution supplied it.
func (f *Flag[T]) Lookup(ctx Context) (T, bool) {
	v := valuesFromCtx(ctx)
	if raw, ok := v[f]; ok {
		return raw.(T), true
	}
	var zero T
	return zero, false
}

// OneOf restricts accepted values using [fmt.Sprint] and registers matching completion candidates.
func (f *Flag[T]) OneOf(allowed ...T) *Flag[T] {
	if len(allowed) == 0 {
		return f
	}
	keys := make([]string, len(allowed))
	set := make(map[string]struct{}, len(allowed))
	for i, a := range allowed {
		keys[i] = fmt.Sprint(a)
		set[keys[i]] = struct{}{}
	}
	f.validate = func(v T) error {
		if _, ok := set[fmt.Sprint(v)]; !ok {
			return fmt.Errorf("value %v is not one of [%s]", v, strings.Join(keys, ", "))
		}
		return nil
	}
	f.completer = StaticValues(keys...)
	return f
}

func (f *Flag[T]) flagName() string        { return f.name }
func (f *Flag[T]) flagShort() rune         { return f.short }
func (f *Flag[T]) flagHelp() string        { return f.help }
func (f *Flag[T]) flagEnv() string         { return f.env }
func (f *Flag[T]) flagRequired() bool      { return f.required }
func (f *Flag[T]) flagHidden() bool        { return f.hidden }
func (f *Flag[T]) flagGroup() string       { return f.group }
func (f *Flag[T]) flagIsBool() bool        { return f.isBool }
func (f *Flag[T]) flagDefaultText() string { return f.defaultStr }
func (f *Flag[T]) flagHasDefault() bool    { return f.hasDefault }

func (f *Flag[T]) flagPlaceholder() string {
	if f.placeholder != "" {
		return f.placeholder
	}
	return defaultPlaceholder(f.name)
}

func (f *Flag[T]) flagApplyString(v values, s string) error {
	if f.isSlice {
		prev, _ := v[f].(T)
		if !f.hasValue(v) {
			// Explicit slice values replace rather than extend the default.
			val, err := f.parse(s)
			if err != nil {
				return f.parseErr(s, err)
			}
			v[f] = val
			return nil
		}
		next, err := f.appendOne(prev, s)
		if err != nil {
			return f.parseErr(s, err)
		}
		v[f] = next
		return nil
	}
	val, err := f.parse(s)
	if err != nil {
		return f.parseErr(s, err)
	}
	v[f] = val
	return nil
}

func (f *Flag[T]) flagApplyEnv(v values, getenv func(string) (string, bool)) error {
	if f.env == "" || f.hasValue(v) {
		return nil
	}
	s, ok := getenv(f.env)
	if !ok {
		return nil
	}
	return f.flagApplyString(v, s)
}

func (f *Flag[T]) flagValidate(v values) error {
	if f.validate == nil {
		return nil
	}
	var val T
	if raw, ok := v[f]; ok {
		val = raw.(T)
	} else if f.hasDefault {
		val = f.defaultVal
	} else {
		return nil
	}
	if err := f.validate(val); err != nil {
		return fmt.Errorf("--%s: %v: %w", f.name, err, ErrValidation)
	}
	return nil
}

func (f *Flag[T]) flagPresent(v values) bool {
	return f.hasValue(v)
}

func (f *Flag[T]) flagCompleter() CompleteFn { return f.completer }

func (f *Flag[T]) hasValue(v values) bool {
	_, ok := v[f]
	return ok
}

func (f *Flag[T]) parseErr(s string, err error) error {
	return fmt.Errorf("--%s=%q: %v: %w", f.name, s, err, ErrUsage)
}

func defaultPlaceholder(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c == '-' {
			c = '_'
		}
		out = append(out, c)
	}
	return string(out)
}
