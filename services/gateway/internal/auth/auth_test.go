package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://issuer.example"
	testAudience = "ecommerce-api"
)

var testNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

var (
	keyOnce sync.Once
	keyA    *rsa.PrivateKey // the key the verifier trusts
	keyB    *rsa.PrivateKey // some other key
)

func keys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if keyA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if keyB, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return keyA, keyB
}

func baseClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   testIssuer,
		"aud":   testAudience,
		"sub":   "alice",
		"exp":   testNow.Add(time.Hour).Unix(),
		"iat":   testNow.Unix(),
		"roles": []string{"admin"},
	}
}

func without(c jwt.MapClaims, keys ...string) jwt.MapClaims {
	out := jwt.MapClaims{}
	for k, v := range c {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func with(c jwt.MapClaims, k string, v any) jwt.MapClaims {
	out := without(c)
	out[k] = v
	return out
}

func sign(t *testing.T, method jwt.SigningMethod, key any, kid string, c jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newVerifier(t *testing.T, ks KeySet) *Verifier {
	t.Helper()
	v, err := NewVerifier(ks, VerifierOptions{Issuer: testIssuer, Audience: testAudience, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidTokenYieldsThePrincipal(t *testing.T) {
	a, _ := keys(t)
	v := newVerifier(t, NewStaticKey(&a.PublicKey, "k1"))

	p, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, a, "k1", baseClaims()))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "alice" || !p.IsAdmin() || len(p.Roles) != 1 {
		t.Fatalf("principal = %+v", p)
	}

	// A token without roles is a plain customer.
	p, err = v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, a, "k1", without(baseClaims(), "roles")))
	if err != nil || p.IsAdmin() || len(p.Roles) != 0 {
		t.Fatalf("principal = %+v, err = %v", p, err)
	}
}

func TestTokensThatMustBeRejected(t *testing.T) {
	a, b := keys(t)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&a.PublicKey)})

	valid := sign(t, jwt.SigningMethodRS256, a, "k1", baseClaims())
	parts := strings.Split(valid, ".")
	tamperedPayload, _ := json.Marshal(with(baseClaims(), "sub", "mallory"))
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tamperedPayload) + "." + parts[2]

	noneToken, err := jwt.NewWithClaims(jwt.SigningMethodNone, baseClaims()).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"not a jwt", "definitely-not-a-jwt"},
		{"signed with another key", sign(t, jwt.SigningMethodRS256, b, "k1", baseClaims())},
		{"payload tampered after signing", tampered},
		{"alg none", noneToken},
		// The classic algorithm-confusion attack: re-sign with HS256 using the
		// PUBLIC key as the HMAC secret.
		{"HS256 signed with the public key", sign(t, jwt.SigningMethodHS256, pubPEM, "k1", baseClaims())},
		{"RS384 instead of RS256", sign(t, jwt.SigningMethodRS384, a, "k1", baseClaims())},
		{"expired", sign(t, jwt.SigningMethodRS256, a, "k1", with(baseClaims(), "exp", testNow.Add(-time.Hour).Unix()))},
		{"expired just beyond the leeway", sign(t, jwt.SigningMethodRS256, a, "k1", with(baseClaims(), "exp", testNow.Add(-31*time.Second).Unix()))},
		{"no expiry", sign(t, jwt.SigningMethodRS256, a, "k1", without(baseClaims(), "exp"))},
		{"not yet valid", sign(t, jwt.SigningMethodRS256, a, "k1", with(baseClaims(), "nbf", testNow.Add(time.Hour).Unix()))},
		{"wrong issuer", sign(t, jwt.SigningMethodRS256, a, "k1", with(baseClaims(), "iss", "https://evil.example"))},
		{"no issuer", sign(t, jwt.SigningMethodRS256, a, "k1", without(baseClaims(), "iss"))},
		{"wrong audience", sign(t, jwt.SigningMethodRS256, a, "k1", with(baseClaims(), "aud", "some-other-api"))},
		{"no audience", sign(t, jwt.SigningMethodRS256, a, "k1", without(baseClaims(), "aud"))},
		{"no subject", sign(t, jwt.SigningMethodRS256, a, "k1", without(baseClaims(), "sub"))},
		{"empty subject", sign(t, jwt.SigningMethodRS256, a, "k1", with(baseClaims(), "sub", ""))},
		{"unknown key id", sign(t, jwt.SigningMethodRS256, a, "other-kid", baseClaims())},
	}

	v := newVerifier(t, NewStaticKey(&a.PublicKey, "k1"))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := v.Verify(context.Background(), tc.token)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken (principal %+v)", err, p)
			}
			if p.Subject != "" {
				t.Errorf("a rejected token must not yield a principal: %+v", p)
			}
		})
	}
}

func TestExpiryLeewayAndMultipleAudiences(t *testing.T) {
	a, _ := keys(t)
	v := newVerifier(t, NewStaticKey(&a.PublicKey, ""))
	ctx := context.Background()

	// Within the 30s leeway an expired token is still accepted (clock skew).
	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, a, "", with(baseClaims(), "exp", testNow.Add(-10*time.Second).Unix()))); err != nil {
		t.Errorf("token expired 10s ago should pass within the leeway: %v", err)
	}
	// An audience list containing ours is fine.
	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, a, "", with(baseClaims(), "aud", []string{"other", testAudience}))); err != nil {
		t.Errorf("audience list: %v", err)
	}
	// A token without a kid works against a single static key.
	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, a, "", baseClaims())); err != nil {
		t.Errorf("no kid: %v", err)
	}
}

func TestNewVerifierRequiresIssuerAndAudience(t *testing.T) {
	a, _ := keys(t)
	ks := NewStaticKey(&a.PublicKey, "")
	if _, err := NewVerifier(ks, VerifierOptions{Audience: "x"}); err == nil {
		t.Error("missing issuer must be rejected")
	}
	if _, err := NewVerifier(ks, VerifierOptions{Issuer: "x"}); err == nil {
		t.Error("missing audience must be rejected")
	}
}

func TestParsePublicKeyPEM(t *testing.T) {
	a, _ := keys(t)
	pkix, _ := x509.MarshalPKIXPublicKey(&a.PublicKey)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)

	for name, data := range map[string][]byte{
		"PKIX":  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix}),
		"PKCS1": pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&a.PublicKey)}),
	} {
		got, err := ParsePublicKeyPEM(data)
		if err != nil || got.N.Cmp(a.N) != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, data := range map[string][]byte{
		"not PEM":          []byte("hello"),
		"private key":      pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(a)}),
		"EC public key":    pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ecDER}),
		"corrupt contents": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("garbage")}),
	} {
		if _, err := ParsePublicKeyPEM(data); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestStaticKeyRejectsAnotherKeyID(t *testing.T) {
	a, _ := keys(t)
	s := NewStaticKey(&a.PublicKey, "k1")
	if _, err := s.Key(context.Background(), "k2"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Key(context.Background(), ""); err != nil {
		t.Fatalf("a token without kid must be accepted: %v", err)
	}
}

// --- JWKS ---

func jwk(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// jwksServer serves a changeable key set and counts requests.
type jwksServer struct {
	*httptest.Server
	mu    sync.Mutex
	keys  []map[string]any
	fail  bool
	calls atomic.Int32
}

func newJWKSServer(t *testing.T, keys ...map[string]any) *jwksServer {
	t.Helper()
	s := &jwksServer{keys: keys}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.calls.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.fail {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": s.keys})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) set(fail bool, keys ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail, s.keys = fail, keys
}

func TestJWKSVerifiesAndCachesKeys(t *testing.T) {
	a, _ := keys(t)
	srv := newJWKSServer(t, jwk("k1", &a.PublicKey))
	v := newVerifier(t, NewJWKS(srv.URL, JWKSOptions{Now: func() time.Time { return testNow }}))

	for range 5 {
		if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, a, "k1", baseClaims())); err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.calls.Load(); n != 1 {
		t.Fatalf("JWKS fetched %d times for 5 verifications, want 1 (cached)", n)
	}
}

func TestJWKSPicksUpRotatedKeysButIsRateLimited(t *testing.T) {
	a, b := keys(t)
	srv := newJWKSServer(t, jwk("old", &a.PublicKey))
	now := testNow
	ks := NewJWKS(srv.URL, JWKSOptions{MinRefresh: time.Minute, Now: func() time.Time { return now }})
	v := newVerifier(t, ks)
	ctx := context.Background()

	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, a, "old", baseClaims())); err != nil {
		t.Fatal(err)
	}

	// The identity provider rotates: a new key appears.
	srv.set(false, jwk("old", &a.PublicKey), jwk("new", &b.PublicKey))
	newToken := sign(t, jwt.SigningMethodRS256, b, "new", baseClaims())

	// Too soon after the last fetch: the unknown key id does not trigger a refresh.
	if _, err := v.Verify(ctx, newToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want rejection until the refresh interval has passed", err)
	}
	if n := srv.calls.Load(); n != 1 {
		t.Fatalf("%d fetches; a flood of unknown key ids must not hammer the identity provider", n)
	}

	now = now.Add(2 * time.Minute)
	if _, err := v.Verify(ctx, newToken); err != nil {
		t.Fatalf("after the interval the new key must be picked up: %v", err)
	}
	if _, err := v.Verify(ctx, sign(t, jwt.SigningMethodRS256, a, "old", baseClaims())); err != nil {
		t.Fatalf("old key must still work: %v", err)
	}
}

func TestJWKSServesKnownKeysFromCacheWhenTheEndpointGoesDown(t *testing.T) {
	a, _ := keys(t)
	srv := newJWKSServer(t, jwk("k1", &a.PublicKey))
	now := testNow
	v := newVerifier(t, NewJWKS(srv.URL, JWKSOptions{TTL: time.Minute, MinRefresh: time.Second, Now: func() time.Time { return now }}))
	token := sign(t, jwt.SigningMethodRS256, a, "k1", baseClaims())

	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatal(err)
	}

	srv.set(true)                   // the endpoint breaks...
	now = now.Add(10 * time.Minute) // ...and the cache is stale
	if _, err := v.Verify(context.Background(), token); err != nil {
		t.Fatalf("a known key must keep working through an outage: %v", err)
	}
}

func TestJWKSOutageWithNoCachedKeyIsOurProblemNotTheCallers(t *testing.T) {
	a, _ := keys(t)
	srv := newJWKSServer(t)
	srv.set(true)
	v := newVerifier(t, NewJWKS(srv.URL, JWKSOptions{Now: func() time.Time { return testNow }}))

	_, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, a, "k1", baseClaims()))
	if !errors.Is(err, ErrKeysUnavailable) {
		t.Fatalf("err = %v, want ErrKeysUnavailable", err)
	}
	if errors.Is(err, ErrInvalidToken) {
		t.Error("an outage must not be reported as an invalid token (401); it is a 503")
	}
}

func TestJWKSIgnoresUnusableKeys(t *testing.T) {
	a, _ := keys(t)
	good := jwk("good", &a.PublicKey)
	srv := newJWKSServer(t,
		map[string]any{"kty": "EC", "kid": "ec", "crv": "P-256", "x": "AA", "y": "AA"},
		map[string]any{"kty": "RSA", "kid": "enc", "use": "enc", "n": good["n"], "e": good["e"]},
		map[string]any{"kty": "RSA", "kid": "rs512", "alg": "RS512", "n": good["n"], "e": good["e"]},
		map[string]any{"kty": "RSA", "kid": "broken", "n": "!!!", "e": "AQAB"},
		good,
	)
	ks := NewJWKS(srv.URL, JWKSOptions{Now: func() time.Time { return testNow }})

	if _, err := ks.Key(context.Background(), "good"); err != nil {
		t.Fatalf("the usable key must be found: %v", err)
	}
	for _, kid := range []string{"ec", "enc", "rs512", "broken"} {
		if _, err := ks.Key(context.Background(), kid); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("%s: err = %v, want ErrUnknownKey", kid, err)
		}
	}
}

func TestJWKSWithASingleKeyAcceptsTokensWithoutKid(t *testing.T) {
	a, _ := keys(t)
	srv := newJWKSServer(t, jwk("only", &a.PublicKey))
	v := newVerifier(t, NewJWKS(srv.URL, JWKSOptions{Now: func() time.Time { return testNow }}))

	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, a, "", baseClaims())); err != nil {
		t.Fatal(err)
	}
}

func TestJWKSWithSeveralKeysRequiresAKid(t *testing.T) {
	a, b := keys(t)
	srv := newJWKSServer(t, jwk("a", &a.PublicKey), jwk("b", &b.PublicKey))
	v := newVerifier(t, NewJWKS(srv.URL, JWKSOptions{Now: func() time.Time { return testNow }}))

	if _, err := v.Verify(context.Background(), sign(t, jwt.SigningMethodRS256, a, "", baseClaims())); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v: with several keys a token must say which one signed it", err)
	}
}

func TestJWKSEmptyOrGarbageResponses(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"no keys": func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, `{"keys":[]}`) },
		"garbage": func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, `not json`) },
	} {
		srv := httptest.NewServer(handler)
		ks := NewJWKS(srv.URL, JWKSOptions{Now: func() time.Time { return testNow }})
		if _, err := ks.Key(context.Background(), "k1"); !errors.Is(err, ErrKeysUnavailable) {
			t.Errorf("%s: err = %v, want ErrKeysUnavailable", name, err)
		}
		srv.Close()
	}
}

// --- DevIssuer ---

func TestDevIssuerTokensVerifyAgainstItsKeySet(t *testing.T) {
	d, err := NewDevIssuer(testIssuer, testAudience, func() time.Time { return testNow })
	if err != nil {
		t.Fatal(err)
	}
	v := newVerifier(t, d.KeySet())

	tok, err := d.Issue("dave", []string{"admin"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), tok)
	if err != nil || p.Subject != "dave" || !p.IsAdmin() {
		t.Fatalf("principal = %+v, err = %v", p, err)
	}

	// Tokens from a different dev issuer (a different key) are rejected.
	other, _ := NewDevIssuer(testIssuer, testAudience, func() time.Time { return testNow })
	foreign, _ := other.Issue("dave", nil, time.Hour)
	if _, err := v.Verify(context.Background(), foreign); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a token from another issuer's key must be rejected: %v", err)
	}
}

func TestDevIssuerValidation(t *testing.T) {
	if _, err := NewDevIssuer("", testAudience, nil); err == nil {
		t.Error("missing issuer must be rejected")
	}
	d, _ := NewDevIssuer(testIssuer, testAudience, func() time.Time { return testNow })
	if _, err := d.Issue("", nil, time.Hour); err == nil {
		t.Error("missing subject must be rejected")
	}
	if _, err := d.Issue("x", nil, 0); err == nil {
		t.Error("a non-positive ttl must be rejected")
	}
}

func TestDevIssuedTokenExpires(t *testing.T) {
	clock := testNow
	d, _ := NewDevIssuer(testIssuer, testAudience, func() time.Time { return clock })
	v, _ := NewVerifier(d.KeySet(), VerifierOptions{Issuer: testIssuer, Audience: testAudience, Now: func() time.Time { return clock }})
	tok, _ := d.Issue("dave", nil, time.Minute)

	clock = testNow.Add(2 * time.Minute)
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want the token to have expired", err)
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Error("no principal expected on a bare context")
	}
	ctx := WithPrincipal(context.Background(), Principal{Subject: "alice", Roles: []string{"support", RoleAdmin}})
	p, ok := FromContext(ctx)
	if !ok || p.Subject != "alice" || !p.IsAdmin() {
		t.Fatalf("principal = %+v, ok = %v", p, ok)
	}
	if (Principal{Subject: "bob", Roles: []string{"support"}}).IsAdmin() {
		t.Error("only the admin role grants admin")
	}
}
