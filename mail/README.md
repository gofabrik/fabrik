# mail

The mail package delivers transactional email through pluggable transports
using only the standard library. Rendering lives in the `mail/templates`
package (imported as `mailtemplates`), which loads paired text and HTML bodies
from one template tree.

## Composing

```go
emailTemplates, err := mailtemplates.Load(templatesFS, "templates/mail")
if err != nil {
	return err
}

content, err := emailTemplates.Render("welcome", data)
if err != nil {
	return err
}

msg := mail.Message{
	From:    "noreply@example.com",
	To:      []string{"ada@example.com"},
	Subject: "Welcome!",
	Text:    content.Text,
	HTML:    content.HTML,
}
```

A template's name is its dir-relative path without the extension: `welcome.txt`
and `welcome.html` are both `welcome`, so one name renders both bodies. Text
bodies use text/template; HTML bodies use html/template with contextual
escaping. Files with a `_`-prefixed base name are shared partials, addressed by
explicit `{{ template "_footer" . }}` calls. Every other `.html` file must have
a `.txt` sibling, so a forgotten text body fails at `Load`, not at send time.

`Render` fills both bodies atomically: each renders into a private buffer and
`Content` is returned only when both succeed. `RenderText` renders the text
body alone and leaves `HTML` empty; a message is text-only because the caller
asks for text, not because a template is missing. Callers may also set the
body fields directly.

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
