package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/common/v1"
	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/health"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/auth"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/ratelimit"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/requestid"
)

// --- fake backends: embed the interface, implement only what a test uses ---

type fakeOrders struct {
	orderv1.OrderServiceClient
	mu     sync.Mutex
	create func(*orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error)
	get    func(*orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error)
	list   func(*orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error)
	cancel func(*orderv1.CancelOrderRequest) (*orderv1.CancelOrderResponse, error)

	createCalls, cancelCalls int
	lastCreate               *orderv1.CreateOrderRequest
	lastList                 *orderv1.ListOrdersRequest
	lastCancel               *orderv1.CancelOrderRequest
}

func (f *fakeOrders) CreateOrder(_ context.Context, in *orderv1.CreateOrderRequest, _ ...grpc.CallOption) (*orderv1.CreateOrderResponse, error) {
	f.mu.Lock()
	f.createCalls++
	f.lastCreate = in
	f.mu.Unlock()
	return f.create(in)
}
func (f *fakeOrders) GetOrder(_ context.Context, in *orderv1.GetOrderRequest, _ ...grpc.CallOption) (*orderv1.GetOrderResponse, error) {
	return f.get(in)
}
func (f *fakeOrders) ListOrders(_ context.Context, in *orderv1.ListOrdersRequest, _ ...grpc.CallOption) (*orderv1.ListOrdersResponse, error) {
	f.mu.Lock()
	f.lastList = in
	f.mu.Unlock()
	return f.list(in)
}
func (f *fakeOrders) CancelOrder(_ context.Context, in *orderv1.CancelOrderRequest, _ ...grpc.CallOption) (*orderv1.CancelOrderResponse, error) {
	f.mu.Lock()
	f.cancelCalls++
	f.lastCancel = in
	f.mu.Unlock()
	return f.cancel(in)
}

type fakeInventory struct {
	inventoryv1.InventoryServiceClient
	get  func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error)
	list func(*inventoryv1.ListStockRequest) (*inventoryv1.ListStockResponse, error)
}

func (f *fakeInventory) GetStock(_ context.Context, in *inventoryv1.GetStockRequest, _ ...grpc.CallOption) (*inventoryv1.GetStockResponse, error) {
	return f.get(in)
}
func (f *fakeInventory) ListStock(_ context.Context, in *inventoryv1.ListStockRequest, _ ...grpc.CallOption) (*inventoryv1.ListStockResponse, error) {
	return f.list(in)
}

type fakeNotifications struct {
	notificationv1.NotificationServiceClient
	mu    sync.Mutex
	list  func(*notificationv1.ListNotificationsRequest) (*notificationv1.ListNotificationsResponse, error)
	calls int
}

func (f *fakeNotifications) ListNotifications(_ context.Context, in *notificationv1.ListNotificationsRequest, _ ...grpc.CallOption) (*notificationv1.ListNotificationsResponse, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.list(in)
}

// --- test harness ---

type harness struct {
	t       *testing.T
	handler http.Handler
	orders  *fakeOrders
	inv     *fakeInventory
	notif   *fakeNotifications
	issuer  *auth.DevIssuer
	logs    *bytes.Buffer
}

type options struct {
	limiter     *ratelimit.Limiter
	noDevToken  bool
	corsOrigins []string
	maxBody     int64
	verifier    TokenVerifier
	metrics     *HTTPMetrics
}

var created = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

func protoOrder(id, customer, status string) *orderv1.Order {
	st := orderv1.OrderStatus_ORDER_STATUS_PENDING
	switch status {
	case "confirmed":
		st = orderv1.OrderStatus_ORDER_STATUS_CONFIRMED
	case "stock_reserved":
		st = orderv1.OrderStatus_ORDER_STATUS_STOCK_RESERVED
	case "cancelled":
		st = orderv1.OrderStatus_ORDER_STATUS_CANCELLED
	}
	return &orderv1.Order{
		Id: id, CustomerId: customer, Status: st,
		Items:      []*orderv1.OrderItem{{Sku: "A", Quantity: 2, UnitPrice: &commonv1.Money{CurrencyCode: "USD", AmountMinor: 1000}}},
		Total:      &commonv1.Money{CurrencyCode: "USD", AmountMinor: 2000},
		CreateTime: timestamppb.New(created), UpdateTime: timestamppb.New(created.Add(time.Minute)),
	}
}

func newHarness(t *testing.T, o ...options) *harness {
	t.Helper()
	var opt options
	if len(o) > 0 {
		opt = o[0]
	}
	issuer, err := auth.NewDevIssuer("iss", "aud", nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier := opt.verifier
	if verifier == nil {
		v, err := auth.NewVerifier(issuer.KeySet(), auth.VerifierOptions{Issuer: "iss", Audience: "aud"})
		if err != nil {
			t.Fatal(err)
		}
		verifier = v
	}

	h := &harness{t: t, orders: &fakeOrders{}, inv: &fakeInventory{}, notif: &fakeNotifications{}, issuer: issuer, logs: &bytes.Buffer{}}
	deps := Deps{
		Orders: h.orders, Inventory: h.inv, Notifications: h.notif,
		Verifier: verifier, Limiter: opt.limiter, Health: health.New(time.Second).Routes(),
		CORSOrigins: opt.corsOrigins, MaxBodyBytes: opt.maxBody, Metrics: opt.metrics,
		Log: slog.New(slog.NewTextHandler(h.logs, nil)),
	}
	if !opt.noDevToken {
		deps.DevIssuer = issuer
	}
	h.handler = NewHandler(deps)
	return h
}

func (h *harness) token(sub string, roles ...string) string {
	h.t.Helper()
	tok, err := h.issuer.Issue(sub, roles, time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

type req struct {
	method, path, body string
	headers            map[string]string
}

func (h *harness) do(r req) *httptest.ResponseRecorder {
	h.t.Helper()
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	hr := httptest.NewRequestWithContext(context.Background(), r.method, r.path, body)
	hr.RemoteAddr = "203.0.113.7:4242"
	if r.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, hr)
	return rec
}

func (h *harness) as(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response is not the expected JSON: %v\n%s", err, rec.Body)
	}
	return v
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[errorBody](t, rec).Error.Code
}

// --- public catalogue ---

func TestProductsArePublicAndMapped(t *testing.T) {
	h := newHarness(t)
	h.inv.list = func(in *inventoryv1.ListStockRequest) (*inventoryv1.ListStockResponse, error) {
		if in.GetPageSize() != 2 || in.GetPageToken() != "tok" {
			t.Errorf("backend got page_size=%d token=%q", in.GetPageSize(), in.GetPageToken())
		}
		return &inventoryv1.ListStockResponse{
			Items: []*inventoryv1.StockItem{{
				Sku: "MUG", Name: "Mug", Available: 7, Reserved: 3,
				UnitPrice: &commonv1.Money{CurrencyCode: "USD", AmountMinor: 1299},
			}},
			NextPageToken: "next",
		}, nil
	}

	rec := h.do(req{method: "GET", path: "/v1/products?page_size=2&page_token=tok"})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `"amount_minor":1299`) {
		t.Errorf("money must be a JSON number, got %s", rec.Body)
	}
	list := decode[productList](t, rec)
	if len(list.Products) != 1 || list.Products[0].SKU != "MUG" || list.Products[0].Available != 7 || list.NextPageToken != "next" {
		t.Fatalf("list = %+v", list)
	}
	if strings.Contains(rec.Body.String(), "reserved") {
		t.Error("internal reservation counts must not be exposed")
	}
}

func TestProductNotFoundUsesTheErrorEnvelopeWithTheRequestID(t *testing.T) {
	h := newHarness(t)
	h.inv.get = func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
		return nil, status.Error(codes.NotFound, "item not found")
	}

	rec := h.do(req{method: "GET", path: "/v1/products/NOPE", headers: map[string]string{"X-Request-Id": "req-42"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	e := decode[errorBody](t, rec).Error
	if e.Code != "not_found" || e.Message != "item not found" || e.RequestID != "req-42" {
		t.Fatalf("error = %+v", e)
	}
}

func TestBadPageSizeIsRejected(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"page_size=abc", "page_size=-1", "page_size=99999999999"} {
		if rec := h.do(req{method: "GET", path: "/v1/products?" + q}); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
}

// --- authentication ---

func TestProtectedRoutesRequireAValidToken(t *testing.T) {
	h := newHarness(t)
	routes := []req{
		{method: "POST", path: "/v1/orders", body: `{"items":[]}`},
		{method: "GET", path: "/v1/orders"},
		{method: "GET", path: "/v1/orders/o-1"},
		{method: "POST", path: "/v1/orders/o-1/cancel"},
		{method: "GET", path: "/v1/orders/o-1/notifications"},
	}

	cases := map[string]map[string]string{
		"no credentials":       nil,
		"garbage token":        {"Authorization": "Bearer not-a-token"},
		"wrong scheme":         {"Authorization": "Basic dXNlcjpwYXNz"},
		"empty bearer":         {"Authorization": "Bearer "},
		"token without scheme": {"Authorization": h.token("alice")},
	}
	for name, headers := range cases {
		for _, r := range routes {
			r.headers = headers
			rec := h.do(r)
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthenticated" {
				t.Errorf("%s %s %s: status = %d, want 401 unauthenticated", name, r.method, r.path, rec.Code)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s %s %s: missing WWW-Authenticate", name, r.method, r.path)
			}
		}
	}
}

func TestAnExpiredTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	// A 1-second token with a leeway of 30s is still valid; use a verifier with no leeway semantics via an old token.
	old, _ := auth.NewDevIssuer("iss", "aud", func() time.Time { return time.Now().Add(-48 * time.Hour) })
	tok, _ := old.Issue("alice", nil, time.Hour)
	if rec := h.do(req{method: "GET", path: "/v1/orders", headers: h.as(tok)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (and the token is signed by another key too)", rec.Code)
	}
}

type brokenVerifier struct{}

func (brokenVerifier) Verify(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, fmt.Errorf("verify token: %w", auth.ErrKeysUnavailable)
}

func TestAnIdentityProviderOutageIs503Not401(t *testing.T) {
	h := newHarness(t, options{verifier: brokenVerifier{}})
	rec := h.do(req{method: "GET", path: "/v1/orders", headers: map[string]string{"Authorization": "Bearer whatever"}})
	if rec.Code != http.StatusServiceUnavailable || errorCode(t, rec) != "unavailable" {
		t.Fatalf("status = %d: our inability to verify tokens must not look like the caller's mistake", rec.Code)
	}
}

func TestPublicRoutesIgnoreABadToken(t *testing.T) {
	h := newHarness(t)
	h.inv.get = func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
		return &inventoryv1.GetStockResponse{Item: &inventoryv1.StockItem{Sku: "A"}}, nil
	}
	if rec := h.do(req{method: "GET", path: "/v1/products/A", headers: map[string]string{"Authorization": "Bearer stale"}}); rec.Code != http.StatusOK {
		t.Fatalf("a public route must not fail because of an expired token in the browser: %d", rec.Code)
	}
}

// --- creating orders ---

func goodCreate(h *harness) {
	h.orders.create = func(in *orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) {
		return &orderv1.CreateOrderResponse{Order: protoOrder("o-1", in.GetCustomerId(), "pending")}, nil
	}
}

func TestCreateOrderTakesTheCustomerFromTheTokenAndReturns202(t *testing.T) {
	h := newHarness(t)
	goodCreate(h)

	rec := h.do(req{
		method: "POST", path: "/v1/orders", body: `{"items":[{"sku":"A","quantity":2},{"sku":"B","quantity":1}]}`,
		headers: map[string]string{"Authorization": "Bearer " + h.token("alice"), "Idempotency-Key": "key-1"},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Location") != "/v1/orders/o-1" {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}

	got := h.orders.lastCreate
	if got.GetCustomerId() != "alice" || got.GetIdempotencyKey() != "key-1" || len(got.GetItems()) != 2 ||
		got.GetItems()[0].GetSku() != "A" || got.GetItems()[0].GetQuantity() != 2 {
		t.Fatalf("backend received %v", got)
	}
	if got.GetItems()[0].GetUnitPrice() != nil {
		t.Error("the gateway must never forward client-supplied prices")
	}

	o := decode[order](t, rec)
	if o.ID != "o-1" || o.Status != "pending" || o.Total.AmountMinor != 2000 || len(o.Items) != 1 || !o.CreatedAt.Equal(created) {
		t.Fatalf("order = %+v", o)
	}
}

func TestACallerCannotOrderOnSomeoneElsesBehalf(t *testing.T) {
	h := newHarness(t)
	goodCreate(h)
	headers := map[string]string{"Authorization": "Bearer " + h.token("mallory"), "Idempotency-Key": "k"}

	// A customer_id (or a price) in the body is not a field of this API.
	for _, body := range []string{
		`{"customer_id":"victim","items":[{"sku":"A","quantity":1}]}`,
		`{"items":[{"sku":"A","quantity":1,"unit_price":{"currency_code":"USD","amount_minor":1}}]}`,
	} {
		rec := h.do(req{method: "POST", path: "/v1/orders", body: body, headers: headers})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
	if h.orders.createCalls != 0 {
		t.Fatalf("the backend was called %d times for rejected requests", h.orders.createCalls)
	}
}

func TestCreateOrderRequestValidation(t *testing.T) {
	h := newHarness(t)
	goodCreate(h)
	tok := "Bearer " + h.token("alice")
	many := `{"items":[` + strings.Repeat(`{"sku":"A","quantity":1},`, 100) + `{"sku":"A","quantity":1}]}`

	tests := []struct {
		name    string
		body    string
		headers map[string]string
		ctype   string
		want    int
	}{
		{"no idempotency key", `{"items":[{"sku":"A","quantity":1}]}`, map[string]string{"Authorization": tok}, "", 400},
		{"idempotency key with a space", `{"items":[{"sku":"A","quantity":1}]}`, map[string]string{"Authorization": tok, "Idempotency-Key": "a b"}, "", 400},
		{"idempotency key too long", `{"items":[{"sku":"A","quantity":1}]}`, map[string]string{"Authorization": tok, "Idempotency-Key": strings.Repeat("k", 129)}, "", 400},
		{"empty body", "", map[string]string{"Authorization": tok, "Idempotency-Key": "k"}, "", 400},
		{"not JSON", `{items`, map[string]string{"Authorization": tok, "Idempotency-Key": "k"}, "", 400},
		{"two JSON values", `{"items":[{"sku":"A","quantity":1}]} {}`, map[string]string{"Authorization": tok, "Idempotency-Key": "k"}, "", 400},
		{"wrong content type", `{"items":[{"sku":"A","quantity":1}]}`, map[string]string{"Authorization": tok, "Idempotency-Key": "k", "Content-Type": "text/plain"}, "", 415},
		{"no items", `{"items":[]}`, map[string]string{"Authorization": tok, "Idempotency-Key": "k"}, "", 400},
		{"too many items", many, map[string]string{"Authorization": tok, "Idempotency-Key": "k"}, "", 400},
		{"a string where a number belongs", `{"items":[{"sku":"A","quantity":"two"}]}`, map[string]string{"Authorization": tok, "Idempotency-Key": "k"}, "", 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(req{method: "POST", path: "/v1/orders", body: tc.body, headers: tc.headers})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestBackendErrorsBecomeTheRightHTTPStatus(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string // "" = must be the generic text, never the backend's
	}{
		{"invalid argument passes its message on", status.Error(codes.InvalidArgument, "unknown SKU NOPE"), 400, "invalid_argument", "unknown SKU NOPE"},
		{"idempotency conflict", status.Error(codes.AlreadyExists, "idempotency key was used with different parameters"), 409, "already_exists", "idempotency key was used with different parameters"},
		{"pricing unavailable", status.Error(codes.Unavailable, "dial tcp 10.0.0.5:9090: connection refused"), 503, "unavailable", ""},
		{"backend timeout", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), 504, "timeout", ""},
		{"internal details are hidden", status.Error(codes.Internal, "pq: password authentication failed for user orders"), 500, "internal", "internal error"},
		{"unexpected non-status error", errors.New("boom 10.1.2.3"), 500, "internal", "internal error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.orders.create = func(*orderv1.CreateOrderRequest) (*orderv1.CreateOrderResponse, error) { return nil, tc.err }

			rec := h.do(req{
				method: "POST", path: "/v1/orders", body: `{"items":[{"sku":"A","quantity":1}]}`,
				headers: map[string]string{"Authorization": "Bearer " + h.token("alice"), "Idempotency-Key": "k"},
			})
			e := decode[errorBody](t, rec).Error
			if rec.Code != tc.wantStatus || e.Code != tc.wantCode {
				t.Fatalf("status = %d code = %q, want %d %q", rec.Code, e.Code, tc.wantStatus, tc.wantCode)
			}
			if tc.wantMessage != "" && e.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", e.Message, tc.wantMessage)
			}
			for _, leak := range []string{"10.0.0.5", "10.1.2.3", "password", "pq:"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("response leaks backend details (%q): %s", leak, rec.Body)
				}
			}
		})
	}
}

// --- reading and cancelling orders: ownership ---

func ordersByID(orders map[string]*orderv1.Order) func(*orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
	return func(in *orderv1.GetOrderRequest) (*orderv1.GetOrderResponse, error) {
		o, ok := orders[in.GetId()]
		if !ok {
			return nil, status.Error(codes.NotFound, "order not found")
		}
		return &orderv1.GetOrderResponse{Order: o}, nil
	}
}

func TestOrdersBelongToTheirOwner(t *testing.T) {
	h := newHarness(t)
	h.orders.get = ordersByID(map[string]*orderv1.Order{"o-alice": protoOrder("o-alice", "alice", "confirmed")})

	own := h.do(req{method: "GET", path: "/v1/orders/o-alice", headers: h.as(h.token("alice"))})
	if own.Code != http.StatusOK || decode[order](t, own).Status != "confirmed" {
		t.Fatalf("owner: %d %s", own.Code, own.Body)
	}

	// Someone else's order must be indistinguishable from a missing one.
	other := h.do(req{method: "GET", path: "/v1/orders/o-alice", headers: h.as(h.token("mallory"))})
	missing := h.do(req{method: "GET", path: "/v1/orders/does-not-exist", headers: h.as(h.token("mallory"))})
	if other.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("other = %d, missing = %d; both must be 404", other.Code, missing.Code)
	}
	if decode[errorBody](t, other).Error.Message != decode[errorBody](t, missing).Error.Message {
		t.Error("a foreign order and a missing one must produce the same message (no probing)")
	}

	admin := h.do(req{method: "GET", path: "/v1/orders/o-alice", headers: h.as(h.token("root", auth.RoleAdmin))})
	if admin.Code != http.StatusOK {
		t.Fatalf("an admin may see any order: %d", admin.Code)
	}
}

func TestCancelOrder(t *testing.T) {
	setup := func(order *orderv1.Order, cancelErr error) *harness {
		h := newHarness(t)
		h.orders.get = ordersByID(map[string]*orderv1.Order{order.GetId(): order})
		h.orders.cancel = func(in *orderv1.CancelOrderRequest) (*orderv1.CancelOrderResponse, error) {
			if cancelErr != nil {
				return nil, cancelErr
			}
			return &orderv1.CancelOrderResponse{Order: protoOrder(in.GetId(), "alice", "cancelled")}, nil
		}
		return h
	}

	t.Run("owner cancels with a reason", func(t *testing.T) {
		h := setup(protoOrder("o-1", "alice", "pending"), nil)
		rec := h.do(req{method: "POST", path: "/v1/orders/o-1/cancel", body: `{"reason":"changed my mind"}`, headers: h.as(h.token("alice"))})
		if rec.Code != http.StatusOK || decode[order](t, rec).Status != "cancelled" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if h.orders.lastCancel.GetReason() != "changed my mind" || h.orders.lastCancel.GetId() != "o-1" {
			t.Errorf("backend received %v", h.orders.lastCancel)
		}
	})

	t.Run("the body is optional", func(t *testing.T) {
		h := setup(protoOrder("o-1", "alice", "pending"), nil)
		if rec := h.do(req{method: "POST", path: "/v1/orders/o-1/cancel", headers: h.as(h.token("alice"))}); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})

	t.Run("someone else's order is never cancelled", func(t *testing.T) {
		h := setup(protoOrder("o-1", "alice", "pending"), nil)
		rec := h.do(req{method: "POST", path: "/v1/orders/o-1/cancel", headers: h.as(h.token("mallory"))})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if h.orders.cancelCalls != 0 {
			t.Fatal("the cancel command must not reach the order service for a foreign order")
		}
	})

	t.Run("a confirmed order cannot be cancelled", func(t *testing.T) {
		h := setup(protoOrder("o-1", "alice", "confirmed"), status.Error(codes.FailedPrecondition, "order can no longer be cancelled"))
		rec := h.do(req{method: "POST", path: "/v1/orders/o-1/cancel", headers: h.as(h.token("alice"))})
		if rec.Code != http.StatusConflict || errorCode(t, rec) != "failed_precondition" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
}

// --- listing ---

func TestListOrdersIsScopedToTheCaller(t *testing.T) {
	h := newHarness(t)
	h.orders.list = func(*orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
		return &orderv1.ListOrdersResponse{Orders: []*orderv1.Order{protoOrder("o-1", "alice", "pending")}, NextPageToken: "next"}, nil
	}

	rec := h.do(req{method: "GET", path: "/v1/orders?page_size=5&page_token=tok", headers: h.as(h.token("alice"))})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := h.orders.lastList; got.GetCustomerId() != "alice" || got.GetPageSize() != 5 || got.GetPageToken() != "tok" {
		t.Fatalf("backend received %v", got)
	}
	if list := decode[orderList](t, rec); len(list.Orders) != 1 || list.NextPageToken != "next" {
		t.Fatalf("list = %+v", list)
	}

	// Asking for your own ID explicitly is fine; for anyone else's it is not.
	if rec := h.do(req{method: "GET", path: "/v1/orders?customer_id=alice", headers: h.as(h.token("alice"))}); rec.Code != http.StatusOK {
		t.Errorf("own customer_id: %d", rec.Code)
	}
	rec = h.do(req{method: "GET", path: "/v1/orders?customer_id=bob", headers: h.as(h.token("alice"))})
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "permission_denied" {
		t.Errorf("someone else's customer_id: %d %s", rec.Code, rec.Body)
	}
}

func TestAdminsMayListEveryonesOrOneCustomers(t *testing.T) {
	h := newHarness(t)
	h.orders.list = func(*orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
		return &orderv1.ListOrdersResponse{}, nil
	}
	admin := h.as(h.token("root", auth.RoleAdmin))

	if rec := h.do(req{method: "GET", path: "/v1/orders", headers: admin}); rec.Code != http.StatusOK || h.orders.lastList.GetCustomerId() != "" {
		t.Fatalf("admin listing everyone: %d, customer filter %q (must be empty, not the admin's own ID)", rec.Code, h.orders.lastList.GetCustomerId())
	}
	if rec := h.do(req{method: "GET", path: "/v1/orders?customer_id=bob", headers: admin}); rec.Code != http.StatusOK || h.orders.lastList.GetCustomerId() != "bob" {
		t.Fatalf("admin listing bob: %d, filter %q", rec.Code, h.orders.lastList.GetCustomerId())
	}
}

// --- notifications ---

func TestOrderNotificationsAreOnlyForTheOwnerOrAdmins(t *testing.T) {
	h := newHarness(t)
	h.orders.get = ordersByID(map[string]*orderv1.Order{"o-1": protoOrder("o-1", "alice", "confirmed")})
	h.notif.list = func(in *notificationv1.ListNotificationsRequest) (*notificationv1.ListNotificationsResponse, error) {
		if in.GetOrderId() != "o-1" {
			t.Errorf("backend asked for order %q", in.GetOrderId())
		}
		return &notificationv1.ListNotificationsResponse{
			Notifications: []*notificationv1.Notification{{
				Id: "n-1", OrderId: "o-1", CustomerId: "alice", Recipient: "alice", Subject: "Your order is confirmed", Body: "Thanks",
				Kind:       notificationv1.NotificationKind_NOTIFICATION_KIND_ORDER_CONFIRMED,
				Channel:    notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL,
				Status:     notificationv1.NotificationStatus_NOTIFICATION_STATUS_SENT,
				CreateTime: timestamppb.New(created), UpdateTime: timestamppb.New(created),
			}},
		}, nil
	}

	rec := h.do(req{method: "GET", path: "/v1/orders/o-1/notifications", headers: h.as(h.token("alice"))})
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	n := decode[notificationList](t, rec).Notifications
	if len(n) != 1 || n[0].Kind != "order_confirmed" || n[0].Channel != "email" || n[0].Status != "sent" {
		t.Fatalf("notifications = %+v", n)
	}
	if strings.Contains(rec.Body.String(), "recipient") {
		t.Error("the recipient address is not part of the public contract")
	}

	callsBefore := h.notif.calls
	if rec := h.do(req{method: "GET", path: "/v1/orders/o-1/notifications", headers: h.as(h.token("mallory"))}); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger: %d, want 404", rec.Code)
	}
	if h.notif.calls != callsBefore {
		t.Error("the notification service must not even be asked about a foreign order")
	}
	if rec := h.do(req{method: "GET", path: "/v1/orders/o-1/notifications", headers: h.as(h.token("root", auth.RoleAdmin))}); rec.Code != http.StatusOK {
		t.Fatalf("an admin: %d", rec.Code)
	}
}

// --- development tokens ---

func TestDevTokenEndpoint(t *testing.T) {
	t.Run("issues a token the gateway accepts", func(t *testing.T) {
		h := newHarness(t)
		h.orders.list = func(*orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
			return &orderv1.ListOrdersResponse{}, nil
		}

		rec := h.do(req{method: "POST", path: "/dev/token", body: `{"subject":"dave","roles":["admin"],"ttl_seconds":600}`})
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		tok := decode[tokenResponse](t, rec)
		if tok.TokenType != "Bearer" || tok.ExpiresIn != 600 || tok.AccessToken == "" {
			t.Fatalf("token response = %+v", tok)
		}
		if rec := h.do(req{method: "GET", path: "/v1/orders", headers: h.as(tok.AccessToken)}); rec.Code != http.StatusOK {
			t.Fatalf("the issued token was rejected: %d", rec.Code)
		}
	})

	t.Run("validation", func(t *testing.T) {
		h := newHarness(t)
		for name, body := range map[string]string{
			"no subject":    `{"roles":["admin"]}`,
			"ttl too long":  `{"subject":"x","ttl_seconds":999999}`,
			"negative ttl":  `{"subject":"x","ttl_seconds":-5}`,
			"unknown field": `{"subject":"x","admin":true}`,
		} {
			if rec := h.do(req{method: "POST", path: "/dev/token", body: body}); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400", name, rec.Code)
			}
		}
	})

	t.Run("does not exist when dev auth is off", func(t *testing.T) {
		h := newHarness(t, options{noDevToken: true})
		rec := h.do(req{method: "POST", path: "/dev/token", body: `{"subject":"dave"}`})
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d: with dev auth off nobody may mint tokens", rec.Code)
		}
	})
}

// --- cross-cutting behaviour ---

func TestRequestIDs(t *testing.T) {
	h := newHarness(t)
	h.inv.get = func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
		return &inventoryv1.GetStockResponse{Item: &inventoryv1.StockItem{Sku: "A"}}, nil
	}

	generated := h.do(req{method: "GET", path: "/v1/products/A"}).Header().Get("X-Request-Id")
	if !requestid.Valid(generated) {
		t.Fatalf("generated request ID %q", generated)
	}
	if h.do(req{method: "GET", path: "/v1/products/A"}).Header().Get("X-Request-Id") == generated {
		t.Error("each request needs its own ID")
	}
	if got := h.do(req{method: "GET", path: "/v1/products/A", headers: map[string]string{"X-Request-Id": "client-id-1"}}).Header().Get("X-Request-Id"); got != "client-id-1" {
		t.Errorf("a safe client ID should be adopted, got %q", got)
	}
	// An ID with spaces or newlines (log forging) is replaced.
	got := h.do(req{method: "GET", path: "/v1/products/A", headers: map[string]string{"X-Request-Id": "evil id; drop"}}).Header().Get("X-Request-Id")
	if got == "evil id; drop" || !requestid.Valid(got) {
		t.Errorf("an unsafe client ID must be replaced, got %q", got)
	}
}

func TestUnknownPathsAndMethodsGetJSONErrors(t *testing.T) {
	h := newHarness(t)

	rec := h.do(req{method: "GET", path: "/v1/nope"})
	if rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found" {
		t.Errorf("unknown path: %d %s", rec.Code, rec.Body)
	}
	rec = h.do(req{method: "DELETE", path: "/v1/products"})
	if rec.Code != http.StatusMethodNotAllowed || errorCode(t, rec) != "method_not_allowed" || rec.Header().Get("Allow") == "" {
		t.Errorf("wrong method: %d allow=%q %s", rec.Code, rec.Header().Get("Allow"), rec.Body)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestAPanicBecomesA500AndTheServerKeepsServing(t *testing.T) {
	h := newHarness(t)
	h.inv.get = func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
		panic("nil pointer in a handler")
	}

	rec := h.do(req{method: "GET", path: "/v1/products/A"})
	if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != "internal" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "nil pointer") {
		t.Error("the panic message must not reach the client")
	}
	if !strings.Contains(h.logs.String(), "panic in handler") {
		t.Error("the panic must be logged")
	}
	if rec := h.do(req{method: "GET", path: "/healthz"}); rec.Code != http.StatusOK {
		t.Errorf("after a panic the next request must still work: %d", rec.Code)
	}
}

func TestRateLimiting(t *testing.T) {
	h := newHarness(t, options{limiter: ratelimit.New(1, 2, ratelimit.Options{Now: func() time.Time { return created }})}) // frozen clock: exactly the burst
	h.orders.list = func(*orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
		return &orderv1.ListOrdersResponse{}, nil
	}
	alice := h.as(h.token("alice"))

	for i := range 2 {
		if rec := h.do(req{method: "GET", path: "/v1/orders", headers: alice}); rec.Code != http.StatusOK {
			t.Fatalf("request %d within the burst: %d", i+1, rec.Code)
		}
	}
	rec := h.do(req{method: "GET", path: "/v1/orders", headers: alice})
	if rec.Code != http.StatusTooManyRequests || errorCode(t, rec) != "rate_limited" {
		t.Fatalf("over the limit: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 must say when to retry")
	}

	// Another user is unaffected, and so are health probes.
	if rec := h.do(req{method: "GET", path: "/v1/orders", headers: h.as(h.token("bob"))}); rec.Code != http.StatusOK {
		t.Errorf("bob must not be limited by alice's traffic: %d", rec.Code)
	}
	for range 10 {
		if rec := h.do(req{method: "GET", path: "/healthz"}); rec.Code != http.StatusOK {
			t.Fatalf("health probes must never be rate limited: %d", rec.Code)
		}
	}
}

func TestUnauthenticatedCallersAreLimitedByAddress(t *testing.T) {
	h := newHarness(t, options{limiter: ratelimit.New(1, 1, ratelimit.Options{Now: func() time.Time { return created }})})
	h.inv.get = func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
		return &inventoryv1.GetStockResponse{Item: &inventoryv1.StockItem{Sku: "A"}}, nil
	}
	if rec := h.do(req{method: "GET", path: "/v1/products/A"}); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d", rec.Code)
	}
	if rec := h.do(req{method: "GET", path: "/v1/products/A"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request from the same address: %d, want 429", rec.Code)
	}
}

func TestOversizedBodiesAreRejected(t *testing.T) {
	h := newHarness(t, options{maxBody: 64})
	goodCreate(h)
	rec := h.do(req{
		method: "POST", path: "/v1/orders", body: `{"items":[{"sku":"` + strings.Repeat("A", 200) + `","quantity":1}]}`,
		headers: map[string]string{"Authorization": "Bearer " + h.token("alice"), "Idempotency-Key": "k"},
	})
	if rec.Code != http.StatusRequestEntityTooLarge || errorCode(t, rec) != "payload_too_large" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestCORS(t *testing.T) {
	h := newHarness(t, options{corsOrigins: []string{"https://admin.example"}})

	pre := h.do(req{method: "OPTIONS", path: "/v1/orders", headers: map[string]string{
		"Origin": "https://admin.example", "Access-Control-Request-Method": "POST",
	}})
	if pre.Code != http.StatusNoContent || pre.Header().Get("Access-Control-Allow-Origin") != "https://admin.example" ||
		!strings.Contains(pre.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key") ||
		!strings.Contains(pre.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", pre.Code, pre.Header())
	}

	other := h.do(req{method: "OPTIONS", path: "/v1/orders", headers: map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": "POST",
	}})
	if other.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("an origin that is not allowed must get no CORS headers: %v", other.Header())
	}

	h.inv.get = func(*inventoryv1.GetStockRequest) (*inventoryv1.GetStockResponse, error) {
		return &inventoryv1.GetStockResponse{Item: &inventoryv1.StockItem{Sku: "A"}}, nil
	}
	actual := h.do(req{method: "GET", path: "/v1/products/A", headers: map[string]string{"Origin": "https://admin.example"}})
	if actual.Header().Get("Access-Control-Allow-Origin") != "https://admin.example" || !strings.Contains(actual.Header().Get("Vary"), "Origin") {
		t.Errorf("actual request: %v", actual.Header())
	}
	if noOrigin := h.do(req{method: "GET", path: "/v1/products/A"}); noOrigin.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("requests without an Origin must get no CORS headers")
	}
}

func TestCORSIsOffByDefault(t *testing.T) {
	h := newHarness(t)
	rec := h.do(req{method: "OPTIONS", path: "/v1/orders", headers: map[string]string{"Origin": "https://anywhere.example", "Access-Control-Request-Method": "POST"}})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no origins configured, yet CORS headers were sent: %v", rec.Header())
	}
}

func TestResponsesAreNotCachedOrSniffed(t *testing.T) {
	h := newHarness(t)
	rec := h.do(req{method: "GET", path: "/healthz"})
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %v", rec.Header())
	}
}

func TestTheAccessLogHasContextButNeverTheToken(t *testing.T) {
	h := newHarness(t)
	h.orders.list = func(*orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
		return &orderv1.ListOrdersResponse{}, nil
	}
	token := h.token("alice")

	h.do(req{method: "GET", path: "/v1/orders", headers: map[string]string{"Authorization": "Bearer " + token, "X-Request-Id": "req-777"}})

	logged := h.logs.String()
	for _, want := range []string{"request_id=req-777", "method=GET", "path=/v1/orders", "status=200", "subject=alice", "duration_ms="} {
		if !strings.Contains(logged, want) {
			t.Errorf("access log is missing %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, token) || strings.Contains(strings.ToLower(logged), "bearer") {
		t.Fatalf("the bearer token must never be logged:\n%s", logged)
	}
}

func TestHealthEndpointsBypassAuthentication(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		if rec := h.do(req{method: "GET", path: p}); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestEnumNames(t *testing.T) {
	if got := enumName(orderv1.OrderStatus_ORDER_STATUS_STOCK_RESERVED, "ORDER_STATUS_"); got != "stock_reserved" {
		t.Errorf("got %q", got)
	}
	if got := enumName(notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_SMS, "NOTIFICATION_CHANNEL_"); got != "sms" {
		t.Errorf("got %q", got)
	}
}
