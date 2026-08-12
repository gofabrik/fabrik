package auth

import "context"

type ctxKey struct{}

// reserved prevents Extra from overriding typed claims or carrying
// token-transport claims.
var reserved = map[string]struct{}{
	"sub": {}, "iss": {}, "name": {}, "email": {}, "email_verified": {},
	"roles": {}, "scope": {}, "auth_time": {},
	"exp": {}, "nbf": {}, "iat": {}, "jti": {}, "aud": {}, "azp": {},
	"nonce": {}, "at_hash": {},
}

// NewContext returns a context carrying a copy of id with reserved Extra
// claims removed. A nil id clears any carried identity.
func NewContext(ctx context.Context, id *Identity) context.Context {
	if id == nil {
		return context.WithValue(ctx, ctxKey{}, (*Identity)(nil))
	}
	c := id.Clone()
	for name := range reserved {
		delete(c.Extra, name)
	}
	return context.WithValue(ctx, ctxKey{}, c)
}

// From returns a copy of the carried identity. It reports false when no
// identity is carried or the carrier was cleared.
func From(ctx context.Context) (*Identity, bool) {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	if id == nil {
		return nil, false
	}
	return id.Clone(), true
}

// IsAuthenticated reports whether ctx carries an identity with a subject.
func IsAuthenticated(ctx context.Context) bool {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	return id.IsAuthenticated()
}
