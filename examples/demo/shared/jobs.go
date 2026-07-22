package shared

import (
	"context"

	"github.com/gofabrik/fabrik/mail"
	"github.com/gofabrik/fabrik/templates"
)

// GreetingNotification is enqueued when a visitor leaves a greeting.
type GreetingNotification struct {
	Name string `json:"name"`
}

//fabrik:job
func SendGreetingNotification(ctx context.Context, mailer Mailer, cfg *MailerConfig, set *templates.Set, n GreetingNotification) error {
	msg := mail.Message{
		From:    cfg.From,
		To:      []string{cfg.To},
		Subject: "New greeting",
	}
	if err := msg.Render(set, "mail/greeting.txt", "mail/greeting", map[string]any{"Name": n.Name}); err != nil {
		return err
	}
	return mailer.Send(ctx, &msg)
}
