package cli

import "fmt"

// UsageError formats a handler error that makes the default renderer append command help.
func UsageError(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrUsage)
}

// ValidationError formats a message and wraps [ErrValidation].
func ValidationError(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrValidation)
}
