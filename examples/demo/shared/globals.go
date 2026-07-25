package shared

import (
	"context"
	"errors"
	"strings"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/web"
)

// ViewHelpers provides shared layout values; fabrik wires its fields.
type ViewHelpers struct {
	Sessions *session.Manager[Session]
	Flash    *flash.Flash
}

// CurrentUser is nil for anonymous visitors and outside a request; any
// other session failure fails the render.
//
//fabrik:templates:global
func (h *ViewHelpers) CurrentUser(ctx context.Context) (*Session, error) {
	ok, err := h.Sessions.Has(ctx)
	if errors.Is(err, session.ErrNoSession) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	s, err := h.Sessions.Get(ctx)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Flashes returns and consumes the pending messages.
//
//fabrik:templates:global
func (h *ViewHelpers) Flashes(ctx context.Context) ([]flash.Message, error) {
	msgs, err := h.Flash.Take(ctx)
	if errors.Is(err, session.ErrNoSession) {
		return nil, nil
	}
	return msgs, err
}

// ActivePrefix marks a nav link active; the root matches only itself.
//
//fabrik:templates:global name=active
func ActivePrefix(ctx context.Context, prefix string) string {
	r := web.RequestFrom(ctx)
	if r == nil {
		return ""
	}
	if prefix == "/" {
		if r.URL.Path == "/" {
			return "active"
		}
		return ""
	}
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return ""
	}
	return "active"
}
