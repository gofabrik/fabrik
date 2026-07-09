package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a [time.Duration] loaded from strings such as "30s" or "5m".
type Duration struct{ d time.Duration }

// Of wraps a time.Duration.
func Of(d time.Duration) Duration { return Duration{d: d} }

// Duration returns the underlying value.
func (d Duration) Duration() time.Duration { return d.d }

// String renders the duration.
func (d Duration) String() string { return d.d.String() }

// UnmarshalYAML parses a duration string from a YAML scalar.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	d.d = parsed
	return nil
}

// MarshalYAML renders the duration back to its string form.
func (d Duration) MarshalYAML() (any, error) { return d.d.String(), nil }
