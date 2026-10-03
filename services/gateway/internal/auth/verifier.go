package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken is returned for any token that must not be trusted: bad
// signature, wrong algorithm, expired, wrong issuer or audience, and so on.
// The reason is deliberately not part of the error shown to callers.
var ErrInvalidToken = errors.New("invalid token")

// Verifier validates bearer tokens.
type Verifier struct {
	keys     KeySet
	issuer   string
	audience string
	leeway   time.Duration
	now      func() time.Time
}

// VerifierOptions configure a Verifier.
type VerifierOptions struct {
	// Issuer and Audience are required: tokens must carry exactly these "iss" and "aud".
	Issuer   string
	Audience string
	// Leeway tolerates small clock differences between issuer and gateway. Default 30s.
	Leeway time.Duration
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
}

// NewVerifier returns a Verifier that trusts the keys in keys.
func NewVerifier(keys KeySet, o VerifierOptions) (*Verifier, error) {
	if o.Issuer == "" || o.Audience == "" {
		return nil, errors.New("issuer and audience are required")
	}
	if o.Leeway == 0 {
		o.Leeway = 30 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Verifier{keys: keys, issuer: o.Issuer, audience: o.Audience, leeway: o.Leeway, now: o.Now}, nil
}

type claims struct {
	jwt.RegisteredClaims
	Roles []string `json:"roles"`
}

// Verify checks the token and returns the caller it identifies.
//
// Only RS256 is accepted. Restricting the algorithm up front is what stops
// the classic attacks: "alg: none", and re-signing a token with HS256 using
// the public key as the HMAC secret. A token must be signed by a trusted key,
// carry the expected issuer and audience, have an expiry that has not passed,
// and name a subject.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keys.Key(ctx, kid)
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(v.leeway),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		// Failing to obtain keys is our problem, not the caller's.
		if errors.Is(err, ErrKeysUnavailable) {
			return Principal{}, fmt.Errorf("verify token: %w", ErrKeysUnavailable)
		}
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if c.Subject == "" {
		return Principal{}, fmt.Errorf("%w: missing subject", ErrInvalidToken)
	}
	return Principal{Subject: c.Subject, Roles: c.Roles}, nil
}
