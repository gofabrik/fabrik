# mail

The mail package composes template-rendered transactional email and delivers it
through pluggable transports using only the standard library.

## Composing

```go
msg := mail.Message{
	From:    "noreply@example.com",
	To:      []string{"ada@example.com"},
	Subject: "Welcome!",
}
if err := msg.Render(set, "mail/welcome.txt", "mail/welcome", data); err != nil {
	return err
}
```

`Render` fills `Text` and `HTML` through a `Renderer`; `*templates.Set`
implements that interface. An empty HTML template name produces text-only mail,
and a render error leaves both fields unchanged. Callers may also set the body
fields directly.

Address parts must be ASCII, so internationalized domains require punycode;
display names may contain UTF-8. If `Message.ID` is set, retries can reuse it as
a stable `Message-ID`. Every built-in transport validates messages before
delivery.

## Sending

```go
var transport mail.Transport = &mail.SMTP{Addr: "smtp.example.com:587"}
if err := transport.Send(ctx, &msg); err != nil {
	return err
}
```

`Transport.Send` is synchronous; implementations treat the message as read-only
and do not retain it. Built-ins:

- `SMTP` requires STARTTLS by default; plaintext and implicit TLS are explicit
  modes. Context cancellation closes the connection, including when a server
  stalls. Setting `Username` enables AUTH PLAIN authentication.
- `Log` reports deliveries through `log/slog` instead of sending them.
- `Recorder` captures deep copies for tests.

The `transporttest` package checks custom transports against the shared
contract.

## Wiring in a fabrik app

A fabrik app selects a transport by aliasing the interface and annotating its
providers:

```go
//fabrik:provider:select mailer.kind
type Mailer = mail.Transport

//fabrik:provider case=log
func NewLogMailer() *mail.Log { return &mail.Log{} }

//fabrik:provider case=smtp
func NewSMTPMailer(cfg *MailerConfig) *mail.SMTP {
	return &mail.SMTP{Addr: cfg.Addr, Username: cfg.Username, Password: cfg.Password}
}
```

`mailer.kind` selects one provider at startup.

## Delivery semantics

`Send` succeeds once the transport knows the message was accepted; later cleanup
failures and cancellations do not reverse that result. Retries can duplicate a
delivery after an ambiguous failure, so keep `Message.ID` stable for receiver
deduplication.
