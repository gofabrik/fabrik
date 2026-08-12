// Package auth carries request identity through contexts.
package auth

import (
	"maps"
	"slices"
	"time"
)

// Identity represents a request identity. Its key is (Issuer, Subject).
type Identity struct {
	Subject       string         // "sub": non-empty means authenticated; unique only within Issuer, (Issuer, Subject) is the identity key
	Issuer        string         // "iss": authority that asserted the identity
	Name          string         // "name": display name
	Email         string         // "email"
	EmailVerified bool           // "email_verified"
	Roles         []string       // "roles"
	Scope         string         // "scope": space-delimited grants
	AuthTime      time.Time      // "auth_time": when the user last authenticated, not when the identity was attached
	Extra         map[string]any // app-defined claims; reserved names are stripped on attach
}

// IsAuthenticated reports whether the identity names a subject. Safe on nil.
func (id *Identity) IsAuthenticated() bool {
	return id != nil && id.Subject != ""
}

// Clone returns a copy whose Roles and Extra map do not alias id. Values
// nested inside Extra still alias.
func (id *Identity) Clone() *Identity {
	if id == nil {
		return nil
	}
	c := *id
	c.Roles = slices.Clone(id.Roles)
	c.Extra = maps.Clone(id.Extra)
	return &c
}
