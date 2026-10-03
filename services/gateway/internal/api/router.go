// Package api is the gateway's REST/JSON surface: routing, middleware and
// handlers that translate HTTP requests into gRPC calls to the backend services.
package api

import (
	"log/slog"
	"net/http"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/auth"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/ratelimit"
)

// DefaultMaxBodyBytes bounds request bodies when Deps.MaxBodyBytes is not set.
const DefaultMaxBodyBytes = 1 << 20

// Deps are the collaborators of the API.
type Deps struct {
	Orders        orderv1.OrderServiceClient
	Inventory     inventoryv1.InventoryServiceClient
	Notifications notificationv1.NotificationServiceClient

	// Verifier checks bearer tokens.
	Verifier TokenVerifier
	// DevIssuer, when set, enables POST /dev/token. Local development only.
	DevIssuer *auth.DevIssuer
	// Limiter limits requests per caller; nil disables rate limiting.
	Limiter *ratelimit.Limiter
	// Health serves /healthz and /readyz.
	Health http.Handler

	// CORSOrigins are the browser origins allowed to call the API.
	CORSOrigins []string
	// MaxBodyBytes bounds request bodies. Defaults to DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// Metrics records request metrics; nil disables them.
	Metrics *HTTPMetrics

	Log *slog.Logger
}

// Handler serves the REST API.
type Handler struct {
	orders        orderv1.OrderServiceClient
	inventory     inventoryv1.InventoryServiceClient
	notifications notificationv1.NotificationServiceClient
	devIssuer     *auth.DevIssuer
	log           *slog.Logger
}

// NewHandler builds the HTTP handler: routes wrapped in the middleware chain.
//
// Outermost first: tracing, request ID, panic recovery, access log, metrics,
// security headers, CORS, authentication (soft), rate limiting, body size
// limit, routes. Metrics sit outside authentication and rate limiting so that
// rejected requests are counted too.
func NewHandler(d Deps) http.Handler {
	if d.MaxBodyBytes == 0 {
		d.MaxBodyBytes = DefaultMaxBodyBytes
	}
	h := &Handler{
		orders: d.Orders, inventory: d.Inventory, notifications: d.Notifications,
		devIssuer: d.DevIssuer, log: d.Log,
	}

	mux := http.NewServeMux()

	// Health probes are public and never rate limited.
	mux.Handle("GET /healthz", d.Health)
	mux.Handle("GET /readyz", d.Health)

	// The catalogue is public.
	mux.HandleFunc("GET /v1/products", h.listProducts)
	mux.HandleFunc("GET /v1/products/{sku}", h.getProduct)

	// Orders belong to the caller.
	mux.HandleFunc("POST /v1/orders", h.requireAuth(h.createOrder))
	mux.HandleFunc("GET /v1/orders", h.requireAuth(h.listOrders))
	mux.HandleFunc("GET /v1/orders/{id}", h.requireAuth(h.getOrder))
	mux.HandleFunc("POST /v1/orders/{id}/cancel", h.requireAuth(h.cancelOrder))
	mux.HandleFunc("GET /v1/orders/{id}/notifications", h.requireAuth(h.listOrderNotifications))

	if d.DevIssuer != nil {
		mux.HandleFunc("POST /dev/token", h.devToken)
	}

	// Unknown paths and wrong methods get the same JSON error shape as everything else.
	handler := jsonNotFound(mux)

	handler = bodyLimit(d.MaxBodyBytes)(handler)
	handler = rateLimit(d.Limiter, "/healthz", "/readyz")(handler)
	handler = authenticate(d.Verifier)(handler)
	handler = cors(d.CORSOrigins)(handler)
	handler = securityHeaders(handler)
	handler = d.Metrics.middleware(mux)(handler)
	handler = accessLog(d.Log)(handler)
	handler = recoverer(d.Log)(handler)
	handler = withRequestID(handler)
	handler = traced(mux)(handler)
	return handler
}

// jsonNotFound makes 404 and 405 responses from the router use the JSON error body.
func jsonNotFound(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		// No route matches. Tell a wrong method apart from an unknown path.
		rec := &probeWriter{header: http.Header{}}
		mux.ServeHTTP(rec, r)
		if rec.status == http.StatusMethodNotAllowed {
			if allow := rec.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this path")
			return
		}
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such endpoint")
	})
}

// probeWriter captures the status and headers the mux would have sent.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header         { return p.header }
func (p *probeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (p *probeWriter) WriteHeader(code int)        { p.status = code }
