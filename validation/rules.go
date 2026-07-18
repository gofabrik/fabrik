package validation

import (
	"cmp"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Required fails on empty or whitespace-only strings.
func Required() Rule[string] { return requiredRule }

var requiredRule Rule[string] = func(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("is required")
	}
	return nil
}

// Email accepts plain addresses with dotted domains. Empty passes; display-name
// and comment forms are rejected.
func Email() Rule[string] { return emailRule }

var emailRule Rule[string] = func(s string) error {
	if s == "" {
		return nil
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return errors.New("must be a valid email address")
	}
	at := strings.LastIndex(s, "@")
	if at < 0 || !strings.Contains(s[at+1:], ".") {
		return errors.New("must be a valid email address")
	}
	return nil
}

// URL requires an absolute URL with scheme and host. Empty passes.
func URL() Rule[string] { return urlRule }

var urlRule Rule[string] = func(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.New("must be a valid URL")
	}
	return nil
}

// MinLen requires at least n runes. Empty passes.
func MinLen(n int) Rule[string] {
	return func(s string) error {
		if s == "" {
			return nil
		}
		if utf8.RuneCountInString(s) < n {
			return fmt.Errorf("must be at least %d characters", n)
		}
		return nil
	}
}

// MaxLen requires at most n runes.
func MaxLen(n int) Rule[string] {
	return func(s string) error {
		if utf8.RuneCountInString(s) > n {
			return fmt.Errorf("must be at most %d characters", n)
		}
		return nil
	}
}

// Pattern requires the string to match re. Empty passes. It panics on nil re.
func Pattern(re *regexp.Regexp) Rule[string] {
	if re == nil {
		panic("validation.Pattern: nil regexp")
	}
	return func(s string) error {
		if s == "" {
			return nil
		}
		if !re.MatchString(s) {
			return errors.New("is not in the expected format")
		}
		return nil
	}
}

// Min requires value >= n. NaN fails.
func Min[T cmp.Ordered](n T) Rule[T] {
	return func(v T) error {
		if v != v { // NaN
			return errors.New("must be a valid number")
		}
		if v < n {
			return fmt.Errorf("must be at least %v", n)
		}
		return nil
	}
}

// Max requires value <= n. NaN fails.
func Max[T cmp.Ordered](n T) Rule[T] {
	return func(v T) error {
		if v != v { // NaN
			return errors.New("must be a valid number")
		}
		if v > n {
			return fmt.Errorf("must be at most %v", n)
		}
		return nil
	}
}

// In requires value to be one of allowed.
func In[T comparable](allowed ...T) Rule[T] {
	return func(v T) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", joinValues(allowed))
	}
}

// joinValues formats allowed values for user-facing messages.
func joinValues[T any](vals []T) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}

// By names a custom or cross-field rule at the call site.
// A plain func(T) error also satisfies Rule.
func By[T any](fn func(T) error) Rule[T] { return fn }
