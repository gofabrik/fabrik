package auth

import "context"

type claimsKey struct{ _ byte }

// WithClaims attaches c to the context; a nil c attaches nothing.
func WithClaims(ctx context.Context, c *ClaimSet) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, claimsKey{}, c)
}

// Claims returns the context's claims, if any.
func Claims(ctx context.Context) (*ClaimSet, bool) {
	c, ok := ctx.Value(claimsKey{}).(*ClaimSet)
	return c, ok
}

// IsAuthenticated reports whether the context carries claims for an
// authenticated principal: a non-empty subject. A client-only token
// has grants but no principal.
func IsAuthenticated(ctx context.Context) bool {
	c, ok := Claims(ctx)
	return ok && c.Subject != ""
}

// HasScope reports whether the context's claims grant scope s.
func HasScope(ctx context.Context, s string) bool {
	c, ok := Claims(ctx)
	if !ok {
		return false
	}
	for _, have := range c.Scopes() {
		if have == s {
			return true
		}
	}
	return false
}

// HasRole reports whether the context's claims grant role r.
func HasRole(ctx context.Context, r string) bool {
	c, ok := Claims(ctx)
	if !ok {
		return false
	}
	for _, have := range c.Roles {
		if have == r {
			return true
		}
	}
	return false
}
