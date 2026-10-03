package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/auth"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/ratelimit"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/requestid"
)

// TokenVerifier validates a bearer token.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (auth.Principal, error)
}

// withRequestID gives every request an ID: the client's if it is safe, else a new one.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestid.Header)
		if !requestid.Valid(id) {
			id = requestid.New()
		}
		w.Header().Set(requestid.Header, id)
		next.ServeHTTP(w, r.WithContext(requestid.With(r.Context(), id)))
	})
}

// recoverer turns a panic in a handler into a 500 instead of a dropped connection.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as documented by net/http
					panic(rec)
				}
				log.Error("panic in handler", "request_id", requestid.From(r.Context()), "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestInfo lets middleware further in (authentication) tell the access log,
// which sits further out, who made the request.
type requestInfo struct{ subject string }

type infoKey struct{}

// statusRecorder remembers the status and size of the response for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// accessLog writes one structured line per request.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			info := &requestInfo{}
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey{}, info)))

			attrs := []any{
				"request_id", requestid.From(r.Context()),
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", clientIP(r),
			}
			if info.subject != "" {
				attrs = append(attrs, "subject", info.subject)
			}
			level := slog.LevelInfo
			if rec.status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			log.Log(r.Context(), level, "request", attrs...)
		})
	}
}

// securityHeaders sets headers appropriate for a JSON API.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// cors allows browser apps (such as the admin UI) on the given origins to call
// the API. "*" allows any origin; requests carry bearer tokens, not cookies,
// so no credentials are shared. With no origins configured, nothing is added.
func cors(origins []string) func(http.Handler) http.Handler {
	allowAny := slices.Contains(origins, "*")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" || len(origins) == 0 || (!allowAny && !slices.Contains(origins, origin)) {
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Add("Vary", "Origin")
			if allowAny {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, Location")

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-Id")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bodyLimit rejects request bodies larger than max bytes.
func bodyLimit(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authResult is what the soft authentication step found. A route that needs a
// caller (requireAuth) decides what to do with it.
type authResult struct {
	principal auth.Principal
	ok        bool
	err       error // set when a token was presented but not accepted
}

type authKey struct{}

// authenticate checks a bearer token if the request carries one, and records
// the outcome. It never rejects by itself, so public routes work with or
// without a token (and with a stale one).
func authenticate(v TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var res authResult
			if header := r.Header.Get("Authorization"); header != "" {
				token, ok := bearerToken(header)
				if !ok {
					res.err = fmt.Errorf("%w: expected 'Authorization: Bearer <token>'", auth.ErrInvalidToken)
				} else if p, err := v.Verify(r.Context(), token); err != nil {
					res.err = err
				} else {
					res.principal, res.ok = p, true
					if info, _ := r.Context().Value(infoKey{}).(*requestInfo); info != nil {
						info.subject = p.Subject
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, res)))
		})
	}
}

func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// requireAuth rejects requests without a valid token and hands the caller to
// the handler.
func (h *Handler) requireAuth(next func(w http.ResponseWriter, r *http.Request, p auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, _ := r.Context().Value(authKey{}).(authResult)
		switch {
		case res.ok:
			next(w, r, res.principal)
		case errors.Is(res.err, auth.ErrKeysUnavailable):
			h.log.Error("cannot verify tokens: signing keys unavailable", "request_id", requestid.From(r.Context()), "error", res.err)
			writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "authentication is temporarily unavailable, please retry")
		case res.err != nil:
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "the access token is invalid or expired")
		default:
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, r, http.StatusUnauthorized, codeUnauthenticated, "authentication is required: send 'Authorization: Bearer <token>'")
		}
	}
}

// rateLimit limits requests per caller: by subject when authenticated, by
// client address otherwise. The paths in exempt (health probes) are never limited.
func rateLimit(l *ratelimit.Limiter, exempt ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slices.Contains(exempt, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			key := "ip:" + clientIP(r)
			if res, _ := r.Context().Value(authKey{}).(authResult); res.ok {
				key = "sub:" + res.principal.Subject
			}
			if ok, retry := l.Allow(key); !ok {
				secs := int(math.Ceil(retry.Seconds()))
				w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
				writeError(w, r, http.StatusTooManyRequests, codeRateLimited, "too many requests, please slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP is the address of the peer. Behind a load balancer this is the
// balancer's address; trusting X-Forwarded-For safely needs knowledge of the
// deployment, so it is deliberately not done here.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
