package shared

import "testing"

func validMailer() MailerConfig {
	return MailerConfig{Kind: "log", From: "noreply@demo.test", To: "owner@demo.test"}
}

func TestMailerConfig_Validate(t *testing.T) {
	if err := validMailer().Validate(); err != nil {
		t.Fatalf("log kind with valid addresses: %v", err)
	}
	smtp := validMailer()
	smtp.Kind, smtp.Addr = "smtp", "relay.demo.test:587"
	if err := smtp.Validate(); err != nil {
		t.Fatalf("valid smtp config: %v", err)
	}

	cases := map[string]func(*MailerConfig){
		"bad from":              func(c *MailerConfig) { c.From = "not-an-address" },
		"bad to":                func(c *MailerConfig) { c.To = "nope" },
		"unicode local from":    func(c *MailerConfig) { c.From = "józsef@example.com" },
		"unicode domain to":     func(c *MailerConfig) { c.To = "owner@例え.jp" },
		"smtp without addr":     func(c *MailerConfig) { c.Kind = "smtp" },
		"smtp bad addr":         func(c *MailerConfig) { c.Kind, c.Addr = "smtp", "no-port" },
		"smtp unknown tls mode": func(c *MailerConfig) { c.Kind, c.Addr, c.TLSMode = "smtp", "h:25", "startls" },
		"smtp password only":    func(c *MailerConfig) { c.Kind, c.Addr, c.Password = "smtp", "h:25", "x" },
	}
	for name, mutate := range cases {
		c := validMailer()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}

	tls := validMailer()
	tls.Kind, tls.Addr, tls.TLSMode = "smtp", "h:25", "plaintext"
	if err := tls.Validate(); err != nil {
		t.Fatalf("explicit tls mode must pass through: %v", err)
	}
	if got := tls.smtp().TLSMode; string(got) != "plaintext" {
		t.Fatalf("TLSMode not passed to the transport: %q", got)
	}
}
