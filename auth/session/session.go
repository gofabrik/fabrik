// Package session authenticates requests using a session-carried identity key.
package session

import (
	"context"
	"errors"
	"net/http"

	"github.com/gofabrik/fabrik/auth"
	fabriksession "github.com/gofabrik/fabrik/session"
)

type record struct {
	Subject string
	Issuer  string
}

// Method authenticates requests from a session-carried identity key.
type Method struct {
	cell *fabriksession.Handle[record]
}

// New registers a Method with r.
func New(r fabriksession.Registry) (*Method, error) {
	cell, err := fabriksession.Use(r, fabriksession.NewKey[record]("github.com/gofabrik/fabrik/auth/session"))
	if err != nil {
		return nil, err
	}
	return &Method{cell: cell}, nil
}

// Establish stores the identity key in the session; login flows call it
// alongside the session manager's Promote. Subject and issuer must be
// non-empty.
func (m *Method) Establish(ctx context.Context, subject, issuer string) error {
	if subject == "" {
		return errors.New("auth/session: empty subject")
	}
	if issuer == "" {
		return errors.New("auth/session: empty issuer")
	}
	return m.cell.Save(ctx, record{Subject: subject, Issuer: issuer})
}

// Clear removes the identity key from the session; logout flows call it
// alongside the session manager's Destroy.
func (m *Method) Clear(ctx context.Context) error {
	return m.cell.Clear(ctx)
}

// Authenticate restores the session-carried identity. Sessionless requests
// abstain; a request outside the session middleware is an error.
func (m *Method) Authenticate(r *http.Request) (*auth.Identity, error) {
	rec, err := m.cell.Get(r.Context())
	if err != nil {
		return nil, err
	}
	if rec.Subject == "" {
		return nil, nil
	}
	return &auth.Identity{Subject: rec.Subject, Issuer: rec.Issuer}, nil
}
