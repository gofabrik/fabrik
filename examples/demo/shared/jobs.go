package shared

import (
	"context"

	"github.com/gofabrik/fabrik/mail"
	mailtemplates "github.com/gofabrik/fabrik/mail/templates"
)

// GreetingNotification is enqueued when a visitor leaves a greeting.
type GreetingNotification struct {
	Name string `json:"name"`
}

// GreetingEmail is the template data for both greeting bodies.
type GreetingEmail struct {
	Name string
}

//fabrik:job
func SendGreetingNotification(ctx context.Context, mailer Mailer, cfg *MailerConfig, emailTemplates *mailtemplates.Renderer, n GreetingNotification) error {
	content, err := emailTemplates.Render("greeting", GreetingEmail{Name: n.Name})
	if err != nil {
		return err
	}
	return mailer.Send(ctx, &mail.Message{
		From:    cfg.From,
		To:      []string{cfg.To},
		Subject: "New greeting",
		Text:    content.Text,
		HTML:    content.HTML,
	})
}
