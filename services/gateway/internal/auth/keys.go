package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// ErrUnknownKey is returned when a token names a signing key we do not trust.
var ErrUnknownKey = errors.New("unknown signing key")

// ErrKeysUnavailable is returned when the signing keys could not be obtained
// (the identity provider's JWKS endpoint is down). It is an operational
// problem, not the caller's: the gateway answers 503, not 401.
var ErrKeysUnavailable = errors.New("signing keys unavailable")

// KeySet looks up the public key that verifies a token.
type KeySet interface {
	// Key returns the key for the token's "kid" header ("" if the token has none).
	Key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// ParsePublicKeyPEM parses an RSA public key in PKIX ("PUBLIC KEY") or PKCS#1
// ("RSA PUBLIC KEY") PEM form.
func ParsePublicKeyPEM(data []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	switch block.Type {
	case "PUBLIC KEY":
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKIX public key: %w", err)
		}
		rsaKey, ok := k.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key is %T, want an RSA key", k)
		}
		return rsaKey, nil
	case "RSA PUBLIC KEY":
		k, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS#1 public key: %w", err)
		}
		return k, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block %q", block.Type)
	}
}

// StaticKey trusts exactly one key. A token without a "kid", or with the
// configured one, is accepted; any other "kid" is rejected.
type StaticKey struct {
	key *rsa.PublicKey
	kid string
}

// NewStaticKey returns a KeySet for a single key. kid may be empty.
func NewStaticKey(key *rsa.PublicKey, kid string) *StaticKey {
	return &StaticKey{key: key, kid: kid}
}

// Key implements KeySet.
func (s *StaticKey) Key(_ context.Context, kid string) (*rsa.PublicKey, error) {
	if kid != "" && s.kid != "" && kid != s.kid {
		return nil, ErrUnknownKey
	}
	return s.key, nil
}

// JWKS fetches signing keys from a JWKS endpoint (RFC 7517), as published by
// an identity provider. Keys are cached, and refreshed when a token names a
// key we have not seen (key rotation), at most once per MinRefresh. If a
// refresh fails, keys already known keep working.
type JWKS struct {
	url    string
	client *http.Client
	ttl    time.Duration
	minGap time.Duration
	now    func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

// JWKSOptions tunes a JWKS key set. Zero values pick the defaults.
type JWKSOptions struct {
	// Client performs the HTTP requests. Defaults to a client with a 5s timeout.
	Client *http.Client
	// TTL is how long fetched keys are considered fresh. Default 10 minutes.
	TTL time.Duration
	// MinRefresh is the shortest time between fetches, so a flood of tokens
	// with unknown key IDs cannot make us hammer the identity provider.
	// Default 30 seconds.
	MinRefresh time.Duration
	// Now is the clock. Defaults to time.Now.
	Now func() time.Time
}

// NewJWKS returns a key set backed by the JWKS at url.
func NewJWKS(url string, o JWKSOptions) *JWKS {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if o.TTL == 0 {
		o.TTL = 10 * time.Minute
	}
	if o.MinRefresh == 0 {
		o.MinRefresh = 30 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &JWKS{url: url, client: o.Client, ttl: o.TTL, minGap: o.MinRefresh, now: o.Now}
}

// Key implements KeySet.
func (j *JWKS) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	now := j.now()
	fresh := j.keys != nil && now.Sub(j.fetchedAt) < j.ttl
	if k, ok := j.lookup(kid); ok && fresh {
		return k, nil
	}

	// Stale, or a key we do not know: refresh, but not more often than minGap.
	var fetchErr error
	if j.keys == nil || now.Sub(j.lastAttempt) >= j.minGap {
		j.lastAttempt = now
		if keys, err := j.fetch(ctx); err != nil {
			fetchErr = err
		} else {
			j.keys, j.fetchedAt = keys, now
		}
	}

	if k, ok := j.lookup(kid); ok {
		return k, nil // also serves a known key from a stale cache when the refresh failed
	}
	if fetchErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeysUnavailable, fetchErr)
	}
	return nil, ErrUnknownKey
}

func (j *JWKS) lookup(kid string) (*rsa.PublicKey, bool) {
	if kid == "" && len(j.keys) == 1 { // a token without kid is fine when there is only one key
		for _, k := range j.keys {
			return k, true
		}
	}
	k, ok := j.keys[kid]
	return k, ok && kid != ""
}

type jwkSet struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

func (j *JWKS) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		// Only RSA signature keys usable with RS256 are of interest.
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pub, err := rsaKey(k.N, k.E)
		if err != nil {
			continue // one bad key must not disable the others
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable RSA signing keys")
	}
	return keys, nil
}

func rsaKey(n, e string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, err
	}
	exp := new(big.Int).SetBytes(eb)
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31-1 {
		return nil, errors.New("unsupported exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(exp.Int64())}, nil
}
