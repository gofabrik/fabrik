package mail

import (
	"context"
	"log/slog"
)

// Log reports validated deliveries through a structured logger instead of
// sending them; a nil Logger uses [slog.Default].
type Log struct {
	Logger *slog.Logger
}

func (l *Log) Send(ctx context.Context, m *Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "mail: would send",
		"from", m.From, "to", m.To, "subject", m.Subject,
		"html", m.HTML != "", "attachments", len(m.Attachments))
	return nil
}
