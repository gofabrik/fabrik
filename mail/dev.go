package mail

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

var devLinkPattern = regexp.MustCompile(`https?://\S+`)

// trimLink strips trailing prose punctuation while preserving balanced URL brackets.
func trimLink(link string) string {
	for len(link) > 0 {
		switch link[len(link)-1] {
		case '.', ',', ';', ':', '!', '?', '\'', '"':
			link = link[:len(link)-1]
		case ')':
			if strings.Count(link, "(") >= strings.Count(link, ")") {
				return link
			}
			link = link[:len(link)-1]
		case ']':
			if strings.Count(link, "[") >= strings.Count(link, "]") {
				return link
			}
			link = link[:len(link)-1]
		case '}':
			if strings.Count(link, "{") >= strings.Count(link, "}") {
				return link
			}
			link = link[:len(link)-1]
		default:
			return link
		}
	}
	return link
}

// Dev logs and prints messages for local development; nil fields use [os.Stdout] and [slog.Default].
type Dev struct {
	Output io.Writer
	Logger *slog.Logger
}

func (d *Dev) Send(ctx context.Context, m *Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}

	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "mail: would send",
		"from", m.From, "to", m.To, "cc", m.Cc, "bcc", m.Bcc, "subject", m.Subject)

	output := d.Output
	if output == nil {
		output = os.Stdout
	}

	var preview strings.Builder
	preview.WriteString("--- mail ---\n")
	fmt.Fprintf(&preview, "subject: %s\n", m.Subject)
	fmt.Fprintf(&preview, "from: %s\n", m.From)
	fmt.Fprintf(&preview, "to: %s\n", strings.Join(m.To, ", "))
	if len(m.Cc) > 0 {
		fmt.Fprintf(&preview, "cc: %s\n", strings.Join(m.Cc, ", "))
	}
	if len(m.Bcc) > 0 {
		fmt.Fprintf(&preview, "bcc: %s\n", strings.Join(m.Bcc, ", "))
	}
	preview.WriteByte('\n')
	preview.WriteString(m.Text)
	if !strings.HasSuffix(m.Text, "\n") {
		preview.WriteByte('\n')
	}
	if m.HTML != "" {
		fmt.Fprintf(&preview, "html: %d bytes\n", len(m.HTML))
	}
	for _, a := range m.Attachments {
		fmt.Fprintf(&preview, "attachment: %s (%d bytes)\n", a.Filename, len(a.Content))
	}
	for _, link := range devLinkPattern.FindAllString(m.Text, -1) {
		fmt.Fprintf(&preview, "link: %s\n", trimLink(link))
	}
	preview.WriteString("--- end mail ---\n")

	written, err := io.WriteString(output, preview.String())
	if err != nil {
		return fmt.Errorf("mail: write dev preview: %w", err)
	}
	if written != preview.Len() {
		return fmt.Errorf("mail: write dev preview: %w", io.ErrShortWrite)
	}
	return nil
}
