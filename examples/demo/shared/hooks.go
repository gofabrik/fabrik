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
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	default:
		return fmt.Errorf("log: invalid format %q (want text or json)", l.Format)
	}
	slog.SetDefault(slog.New(handler))
	return nil
}
