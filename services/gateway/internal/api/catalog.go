package api

import (
	"net/http"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/auth"
)

// listProducts lists the catalogue. It is public.
func (h *Handler) listProducts(w http.ResponseWriter, r *http.Request) {
	size, ok := queryPageSize(w, r)
	if !ok {
		return
	}
	resp, err := h.inventory.ListStock(r.Context(), &inventoryv1.ListStockRequest{PageSize: size, PageToken: r.URL.Query().Get("page_token")})
	if err != nil {
		h.writeGRPCError(w, r, "list products", err)
		return
	}
	out := productList{Products: make([]product, len(resp.GetItems())), NextPageToken: resp.GetNextPageToken()}
	for i, it := range resp.GetItems() {
		out.Products[i] = toProduct(it)
	}
	writeJSON(w, http.StatusOK, out)
}

// getProduct returns one product by SKU. It is public.
func (h *Handler) getProduct(w http.ResponseWriter, r *http.Request) {
	resp, err := h.inventory.GetStock(r.Context(), &inventoryv1.GetStockRequest{Sku: r.PathValue("sku")})
	if err != nil {
		h.writeGRPCError(w, r, "get product", err)
		return
	}
	writeJSON(w, http.StatusOK, toProduct(resp.GetItem()))
}

// listOrderNotifications shows what the customer was told about an order. Only
// the order's owner or an admin may see it.
func (h *Handler) listOrderNotifications(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	size, ok := queryPageSize(w, r)
	if !ok {
		return
	}
	o := h.loadOwnedOrder(w, r, p, "list order notifications")
	if o == nil {
		return
	}

	resp, err := h.notifications.ListNotifications(r.Context(), &notificationv1.ListNotificationsRequest{
		OrderId: o.GetId(), PageSize: size, PageToken: r.URL.Query().Get("page_token"),
	})
	if err != nil {
		h.writeGRPCError(w, r, "list notifications", err)
		return
	}
	out := notificationList{Notifications: make([]notification, len(resp.GetNotifications())), NextPageToken: resp.GetNextPageToken()}
	for i, n := range resp.GetNotifications() {
		out.Notifications[i] = toNotification(n)
	}
	writeJSON(w, http.StatusOK, out)
}
