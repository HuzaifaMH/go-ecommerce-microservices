// Package requestid carries a per-request ID through the context, so the
// gateway's logs, its responses and its calls to other services can all be
// tied to one request.
package requestid

import (
	"context"

	"github.com/google/uuid"
)

// Header is the HTTP header (and gRPC metadata key) that carries the ID.
const Header = "X-Request-Id"

// MetadataKey is Header as a lower-case gRPC metadata key.
const MetadataKey = "x-request-id"

// MaxLength bounds IDs accepted from clients.
const MaxLength = 64

type key struct{}

// With returns ctx carrying id.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// From returns the request ID in ctx, or "".
func From(ctx context.Context) string {
	id, _ := ctx.Value(key{}).(string)
	return id
}

// New returns a fresh ID.
func New() string { return uuid.NewString() }

// Valid reports whether a client-supplied ID is safe to adopt: non-empty, not
// too long, and made only of letters, digits, '-', '_' and '.'. Anything else
// could be used to forge log lines or smuggle headers.
func Valid(id string) bool {
	if id == "" || len(id) > MaxLength {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
