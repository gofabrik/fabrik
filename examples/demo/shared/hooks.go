package shared

import (
	"fmt"
	"log/slog"
	"os"
)

//fabrik:hook setup
func InitLogger(l *LogConfig) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(l.Level)); err != nil {
		return err
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch l.Format {
	case LogFormatText:
		handler = slog.NewTextHandler(os.Stderr, opts)
	case LogFormatJSON:
		handler = slog.NewJSONHandler(os.Stderr, opts)
	default:
		return fmt.Errorf("log: invalid format %q (want %s or %s)", l.Format, LogFormatText, LogFormatJSON)
	}
	slog.SetDefault(slog.New(handler))
	return nil
}
