package shared

import (
	"log/slog"
	"os"
)

//fabrik:hook setup
func InitLogger(l *LogConfig) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(l.Level)); err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return nil
}
