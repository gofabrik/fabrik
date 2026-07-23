package shared

import (
	"errors"

	"github.com/gofabrik/fabrik/config"
	"github.com/gofabrik/fabrik/mail"
)

//fabrik:config http
type HTTPConfig struct {
	Addr string `yaml:"addr" env:"DEMO_HTTP_ADDR" default:":8080"`
}

//fabrik:config jobs
type JobsConfig struct {
	Concurrency     int             `yaml:"concurrency" env:"DEMO_JOBS_CONCURRENCY" default:"2"`
	ShutdownTimeout config.Duration `yaml:"shutdown_timeout" env:"DEMO_JOBS_SHUTDOWN_TIMEOUT" default:"15s"`
}

//fabrik:config database
type DatabaseConfig struct {
	Path string `yaml:"path" env:"DEMO_DATABASE_PATH" default:"demo.db"`
}

//fabrik:config log
type LogConfig struct {
	Level string `yaml:"level" env:"DEMO_LOG_LEVEL" default:"info"`
}

//fabrik:config crossorigin
type CrossOriginConfig struct {
	TrustedOrigins []string `yaml:"trusted_origins" env:"DEMO_CROSSORIGIN_TRUSTED_ORIGINS"`
}

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

// Validate rejects configurations whose notifications could never be
// delivered, using the mail library's own rules.
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

//fabrik:config storage
type StorageConfig struct {
	Path string `yaml:"path" env:"DEMO_STORAGE_PATH" default:"demo-files"`
}

func (c StorageConfig) Validate() error {
	if c.Path == "" {
		return errors.New("storage.path is required")
	}
	return nil
}
