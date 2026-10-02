package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// DevIssuer mints RS256 tokens with a throwaway key pair generated at start-up.
//
// It exists so the system can be tried locally without an identity provider.
// The private key lives only in memory and changes on every start. It must
// never be enabled in production; the gateway only builds one when DEV_AUTH
// is set.
type DevIssuer struct {
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
	now      func() time.Time
}

// NewDevIssuer generates a key pair and returns an issuer for the given issuer and audience.
func NewDevIssuer(issuer, audience string, now func() time.Time) (*DevIssuer, error) {
	if issuer == "" || audience == "" {
		return nil, errors.New("issuer and audience are required")
	}
	if now == nil {
		now = time.Now
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate dev key: %w", err)
	}
	return &DevIssuer{key: key, kid: "dev-" + uuid.NewString()[:8], issuer: issuer, audience: audience, now: now}, nil
}

// KeySet returns the key set that verifies tokens from this issuer.
func (d *DevIssuer) KeySet() KeySet {
	return NewStaticKey(&d.key.PublicKey, d.kid)
}

// Issue returns a signed token for subject with the given roles, valid for ttl.
func (d *DevIssuer) Issue(subject string, roles []string, ttl time.Duration) (string, error) {
	if subject == "" {
		return "", errors.New("subject is required")
	}
	if ttl <= 0 {
		return "", errors.New("ttl must be positive")
	}
	now := d.now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    d.issuer,
			Audience:  jwt.ClaimStrings{d.audience},
			Subject:   subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        uuid.NewString(),
		},
		Roles: roles,
	})
	tok.Header["kid"] = d.kid
	signed, err := tok.SignedString(d.key)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}
