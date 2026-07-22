package shared

import (
	"context"
	"errors"

	"github.com/gofabrik/fabrik/mail"
	"github.com/gofabrik/fabrik/templates"
)

//fabrik:config mailer
type MailerConfig struct {
	Kind     string `yaml:"kind" env:"DEMO_MAILER_KIND" default:"log"`
	From     string `yaml:"from" env:"DEMO_MAILER_FROM" default:"noreply@demo.test"`
	To       string `yaml:"to" env:"DEMO_MAILER_TO" default:"owner@demo.test"`
	Addr     string `yaml:"addr" env:"DEMO_MAILER_ADDR"`
	Username string `yaml:"username" env:"DEMO_MAILER_USERNAME"`
	Password string `yaml:"password" env:"DEMO_MAILER_PASSWORD" secret:"true"`
	TLSMode  string `yaml:"tls_mode" env:"DEMO_MAILER_TLS_MODE"`
}

// Validate rejects addresses and SMTP settings that cannot be used for delivery.
func (c MailerConfig) Validate() error {
	probe := mail.Message{From: c.From, To: []string{c.To}, Subject: "config", Text: "config"}
	if err := probe.Validate(); err != nil {
		return err
	}
	if c.Kind == "smtp" {
		if c.Addr == "" {
			return errors.New("mailer.addr is required when mailer.kind is smtp")
		}
		return c.smtp().Validate()
	}
	return nil
}

func (c MailerConfig) smtp() *mail.SMTP {
	return &mail.SMTP{
		Addr:     c.Addr,
		Username: c.Username,
		Password: c.Password,
		TLSMode:  mail.TLSMode(c.TLSMode),
	}
}

// Mailer is the transport selected by mailer.kind.
//
//fabrik:provider:select mailer.kind
type Mailer = mail.Transport

//fabrik:provider case=log
func NewLogMailer() *mail.Log { return &mail.Log{} }

//fabrik:provider case=smtp
func NewSMTPMailer(cfg *MailerConfig) *mail.SMTP {
	return cfg.smtp()
}

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
