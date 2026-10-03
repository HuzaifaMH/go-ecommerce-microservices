package api

import (
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/auth"
)

const (
	maxOrderLines     = 100
	maxIdempotencyKey = 128
)

// createOrder places an order for the authenticated caller.
//
// The customer is always taken from the token, never from the request: the
// body has no customer field (and unknown fields are rejected), so a caller
// cannot order on someone else's behalf. Prices are not accepted either; the
// order service prices the order from the catalogue.
func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	key := r.Header.Get("Idempotency-Key")
	if !printableASCII(key, maxIdempotencyKey) {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument,
			"the Idempotency-Key header is required (1-128 visible ASCII characters); repeating a request with the same key returns the original order")
		return
	}

	var req createOrderRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	if len(req.Items) == 0 || len(req.Items) > maxOrderLines {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "items must contain between 1 and 100 entries")
		return
	}
	items := make([]*orderv1.OrderItem, len(req.Items))
	for i, it := range req.Items {
		items[i] = &orderv1.OrderItem{Sku: it.SKU, Quantity: it.Quantity}
	}

	resp, err := h.orders.CreateOrder(r.Context(), &orderv1.CreateOrderRequest{
		CustomerId: p.Subject, IdempotencyKey: key, Items: items,
	})
	if err != nil {
		h.writeGRPCError(w, r, "create order", err)
		return
	}

	// 202: the order is accepted and being processed; poll it for the outcome.
	w.Header().Set("Location", "/v1/orders/"+resp.GetOrder().GetId())
	writeJSON(w, http.StatusAccepted, toOrder(resp.GetOrder()))
}

// loadOwnedOrder fetches an order and checks that the caller may see it. Other
// people's orders look exactly like missing ones, so IDs cannot be probed. It
// writes the error response itself and returns nil on failure.
func (h *Handler) loadOwnedOrder(w http.ResponseWriter, r *http.Request, p auth.Principal, op string) *orderv1.Order {
	resp, err := h.orders.GetOrder(r.Context(), &orderv1.GetOrderRequest{Id: r.PathValue("id")})
	if err != nil {
		h.writeGRPCError(w, r, op, err)
		return nil
	}
	if o := resp.GetOrder(); p.IsAdmin() || o.GetCustomerId() == p.Subject {
		return o
	}
	h.writeGRPCError(w, r, op, status.Error(codes.NotFound, "order not found"))
	return nil
}

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if o := h.loadOwnedOrder(w, r, p, "get order"); o != nil {
		writeJSON(w, http.StatusOK, toOrder(o))
	}
}

// listOrders returns the caller's orders, newest first. An admin may list
// everyone's, or one customer's with ?customer_id=.
func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	size, ok := queryPageSize(w, r)
	if !ok {
		return
	}

	customer := p.Subject
	if p.IsAdmin() {
		customer = r.URL.Query().Get("customer_id") // empty: every customer
	} else if other := r.URL.Query().Get("customer_id"); other != "" && other != p.Subject {
		writeError(w, r, http.StatusForbidden, "permission_denied", "you can only list your own orders")
		return
	}

	resp, err := h.orders.ListOrders(r.Context(), &orderv1.ListOrdersRequest{
		CustomerId: customer, PageSize: size, PageToken: r.URL.Query().Get("page_token"),
	})
	if err != nil {
		h.writeGRPCError(w, r, "list orders", err)
		return
	}
	out := orderList{Orders: make([]order, len(resp.GetOrders())), NextPageToken: resp.GetNextPageToken()}
	for i, o := range resp.GetOrders() {
		out.Orders[i] = toOrder(o)
	}
	writeJSON(w, http.StatusOK, out)
}

// cancelOrder cancels an order that has not been confirmed yet.
func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req cancelOrderRequest
	if !decodeJSON(w, r, &req, true) {
		return
	}
	if o := h.loadOwnedOrder(w, r, p, "cancel order"); o == nil {
		return
	}

	resp, err := h.orders.CancelOrder(r.Context(), &orderv1.CancelOrderRequest{Id: r.PathValue("id"), Reason: req.Reason})
	if err != nil {
		h.writeGRPCError(w, r, "cancel order", err)
		return
	}
	writeJSON(w, http.StatusOK, toOrder(resp.GetOrder()))
}
