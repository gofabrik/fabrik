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
// Flash keeps its data in a private session cell and commits with the
// request's other session writes.
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

// Add appends a message for the next render.
//
// Concurrent requests on one session are last-writer-wins, matching
// staged session writes.
func (f *Flash) Add(ctx context.Context, kind, text string) error {
	d, err := f.cell.Get(ctx)
	if err != nil {
		return err
	}
	d.Messages = append(d.Messages, Message{Kind: kind, Text: text})
	return f.cell.Save(ctx, d)
}

// Take returns pending messages and clears them.
func (f *Flash) Take(ctx context.Context) ([]Message, error) {
	d, err := f.cell.Get(ctx)
	if err != nil {
		return nil, err
	}
	if len(d.Messages) == 0 {
		return nil, nil
	}
	if err := f.cell.Clear(ctx); err != nil {
		return nil, err
	}
	return d.Messages, nil
}

// Peek returns the pending messages without consuming them.
func (f *Flash) Peek(ctx context.Context) ([]Message, error) {
	d, err := f.cell.Get(ctx)
	if err != nil {
		return nil, err
	}
	return d.Messages, nil
}

// Clear drops pending messages without rendering them.
func (f *Flash) Clear(ctx context.Context) error {
	return f.cell.Clear(ctx)
}
