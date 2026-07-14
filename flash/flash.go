// Package flash stores one-shot messages in the visitor's session.
//
//	fl, err := flash.New(sessions) // any *session.Manager[T]
//
//	// in the handler that acts:
//	fl.Add(ctx, "success", "Profile saved.")
//
//	// in the handler that renders:
//	msgs, err := fl.Take(ctx)
//
// Flash keeps its data in a private session cell. Add, Take, and Clear commit
// immediately with optimistic CAS, retried up to the session's MaxRetries. A
// successful call preserves one-shot delivery; retry exhaustion returns
// session.ErrVersionConflict.
//
// The library is standalone: net/http and any mux, no framework
// required.
package flash

import (
	"context"

	"github.com/gofabrik/fabrik/session"
)

// Message is one pending notice. Kind is application-defined.
type Message struct {
	Kind string
	Text string
}

type data struct {
	Messages []Message
}

var key = session.NewKey[data]("github.com/gofabrik/fabrik/flash")

// Flash stores one-shot messages in the visitor's session.
type Flash struct {
	cell *session.Handle[data]
}

// New registers flash's private session cell.
func New(m session.Registry) (*Flash, error) {
	h, err := session.Use(m, key)
	if err != nil {
		return nil, err
	}
	return &Flash{cell: h}, nil
}

// Add appends a message for the next render. Concurrent successful calls all
// survive; retry exhaustion returns session.ErrVersionConflict.
func (f *Flash) Add(ctx context.Context, kind, text string) error {
	return f.cell.Update(ctx, func(d *data) error {
		d.Messages = append(d.Messages, Message{Kind: kind, Text: text})
		return nil
	})
}

// Take returns pending messages and clears them, atomically: a message is
// delivered to exactly one Take. Like Add it retries a CAS conflict up to the
// session's MaxRetries, then returns session.ErrVersionConflict.
func (f *Flash) Take(ctx context.Context) ([]Message, error) {
	// Empty reads do not write or mint a session.
	d, err := f.cell.Get(ctx)
	if err != nil {
		return nil, err
	}
	if len(d.Messages) == 0 {
		return nil, nil
	}
	// Update has no delete form; clearing writes an empty cell.
	var taken []Message
	if err := f.cell.Update(ctx, func(d *data) error {
		taken = d.Messages
		d.Messages = nil
		return nil
	}); err != nil {
		return nil, err
	}
	return taken, nil
}

// Peek returns the pending messages without consuming them.
func (f *Flash) Peek(ctx context.Context) ([]Message, error) {
	d, err := f.cell.Get(ctx)
	if err != nil {
		return nil, err
	}
	return d.Messages, nil
}

// Clear drops pending messages without rendering them. It is a no-op, with no
// write, when there is nothing pending.
func (f *Flash) Clear(ctx context.Context) error {
	d, err := f.cell.Get(ctx)
	if err != nil {
		return err
	}
	if len(d.Messages) == 0 {
		return nil
	}
	return f.cell.Update(ctx, func(d *data) error {
		d.Messages = nil
		return nil
	})
}
