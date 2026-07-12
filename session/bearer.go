package session

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Bearer is a [Token] that reads the session ID from a request
// header (default "Authorization: Bearer <sid>") and pushes any
// minted or rotated SID to a response header (default
// "X-Session-Token").
//
// Bearer clients must persist emitted response tokens themselves.
type Bearer struct {
	// ReadHeader is the request header parsed for the inbound
	// session ID. Empty defaults to "Authorization".
	ReadHeader string

	// Scheme is the prefix expected on the read header value (as
	// in "Bearer <sid>"). Empty defaults to "Bearer". Matching is
	// case-insensitive.
	Scheme string

	// WriteHeader is the response header used to convey minted or
	// rotated session IDs back to the client. Empty defaults to
	// "X-Session-Token".
	WriteHeader string
}

const (
	defaultBearerReadHeader  = "Authorization"
	defaultBearerScheme      = "Bearer"
	defaultBearerWriteHeader = "X-Session-Token"
)

func (b Bearer) readHeader() string {
	if b.ReadHeader == "" {
		return defaultBearerReadHeader
	}
	return b.ReadHeader
}

func (b Bearer) scheme() string {
	if b.Scheme == "" {
		return defaultBearerScheme
	}
	return b.Scheme
}

func (b Bearer) writeHeader() string {
	if b.WriteHeader == "" {
		return defaultBearerWriteHeader
	}
	return b.WriteHeader
}

// Read extracts the session ID from the configured request header.
// The header value must be "<Scheme> <sid>" with any single run of
// whitespace as the separator; the scheme match is case-insensitive.
func (b Bearer) Read(r *http.Request) (string, bool) {
	raw := r.Header.Get(b.readHeader())
	if raw == "" {
		return "", false
	}
	parts := strings.Fields(raw)
	if len(parts) != 2 {
		return "", false
	}
	if !strings.EqualFold(parts[0], b.scheme()) {
		return "", false
	}
	return parts[1], true
}

// Write sets the configured response header to sid.
func (b Bearer) Write(w http.ResponseWriter, sid string, _ TokenWriteOptions) {
	w.Header().Set(b.writeHeader(), sid)
}

func (b Bearer) Clear(w http.ResponseWriter) {
	// Empty signals "drop your token"; absence means no change.
	w.Header().Set(b.writeHeader(), "")
}

// Multi composes multiple [Token] implementations. Read returns the
// first non-empty value the members produce, in order. Write and
// Clear are applied to every member.
//
// Use it for apps that accept session IDs over more than one
// transport.
type Multi []Token

// Validate reports composition mistakes before first request.
func (m Multi) Validate() error {
	if len(m) == 0 {
		return errors.New("session: Multi token has no members")
	}
	for i, t := range m {
		if t == nil {
			return fmt.Errorf("session: Multi token member %d is nil", i)
		}
		if v, ok := t.(interface{ Validate() error }); ok {
			if err := v.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Read returns the first member hit in declaration order.
func (m Multi) Read(r *http.Request) (string, bool) {
	for _, t := range m {
		if sid, ok := t.Read(r); ok {
			return sid, true
		}
	}
	return "", false
}

// Write fans the same sid out to every member.
func (m Multi) Write(w http.ResponseWriter, sid string, opts TokenWriteOptions) {
	for _, t := range m {
		t.Write(w, sid, opts)
	}
}

// Clear fans the call out to every member in declaration order.
func (m Multi) Clear(w http.ResponseWriter) {
	for _, t := range m {
		t.Clear(w)
	}
}
