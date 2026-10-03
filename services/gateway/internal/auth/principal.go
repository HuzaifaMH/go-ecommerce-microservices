// Package auth authenticates API callers from RS256-signed JWT bearer tokens.
//
// The gateway only verifies tokens; it never issues them in production. Keys
// come from a PEM public key or a JWKS URL (a real identity provider). For
// local development a DevIssuer can mint tokens with a throwaway key pair.
package auth

import (
	"context"
	"slices"
)

// RoleAdmin may see every customer's orders and notifications.
const RoleAdmin = "admin"

// Principal is the authenticated caller.
type Principal struct {
	// Subject is the caller's stable ID (the token's "sub" claim). It is used
	// as the customer ID on orders.
	Subject string
	// Roles come from the token's "roles" claim.
	Roles []string
}

// IsAdmin reports whether the caller has the admin role.
func (p Principal) IsAdmin() bool {
	return slices.Contains(p.Roles, RoleAdmin)
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the caller authenticated for this request, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
