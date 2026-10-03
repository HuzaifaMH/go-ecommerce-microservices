package api

import (
	"fmt"
	"strings"
	"time"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	notificationv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/notification/v1"
	orderv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/order/v1"
)

// The REST contract. These types are deliberately separate from the protobuf
// messages: the public JSON can stay stable while internal contracts evolve,
// money stays a JSON number (protobuf's JSON mapping would turn int64 into a
// string), and internal fields are not exposed.

type money struct {
	CurrencyCode string `json:"currency_code"`
	AmountMinor  int64  `json:"amount_minor"`
}

type orderItem struct {
	SKU       string `json:"sku"`
	Quantity  int32  `json:"quantity"`
	UnitPrice money  `json:"unit_price"`
}

type order struct {
	ID           string      `json:"id"`
	CustomerID   string      `json:"customer_id"`
	Items        []orderItem `json:"items"`
	Total        money       `json:"total"`
	Status       string      `json:"status"`
	CancelReason string      `json:"cancel_reason,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type orderList struct {
	Orders        []order `json:"orders"`
	NextPageToken string  `json:"next_page_token,omitempty"`
}

type product struct {
	SKU       string `json:"sku"`
	Name      string `json:"name"`
	UnitPrice money  `json:"unit_price"`
	Available int32  `json:"available"`
}

type productList struct {
	Products      []product `json:"products"`
	NextPageToken string    `json:"next_page_token,omitempty"`
}

type notification struct {
	ID            string    `json:"id"`
	OrderID       string    `json:"order_id"`
	Kind          string    `json:"kind"`
	Channel       string    `json:"channel"`
	Subject       string    `json:"subject"`
	Body          string    `json:"body"`
	Status        string    `json:"status"`
	FailureReason string    `json:"failure_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type notificationList struct {
	Notifications []notification `json:"notifications"`
	NextPageToken string         `json:"next_page_token,omitempty"`
}

type createOrderRequest struct {
	Items []createOrderItem `json:"items"`
}

type createOrderItem struct {
	SKU      string `json:"sku"`
	Quantity int32  `json:"quantity"`
}

type cancelOrderRequest struct {
	Reason string `json:"reason"`
}

// enumName turns a protobuf enum like ORDER_STATUS_STOCK_RESERVED into the
// public value "stock_reserved".
func enumName(e fmt.Stringer, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(e.String(), prefix))
}

func toMoney(currency string, amount int64) money {
	return money{CurrencyCode: currency, AmountMinor: amount}
}

func toOrder(o *orderv1.Order) order {
	items := make([]orderItem, len(o.GetItems()))
	for i, it := range o.GetItems() {
		items[i] = orderItem{
			SKU: it.GetSku(), Quantity: it.GetQuantity(),
			UnitPrice: toMoney(it.GetUnitPrice().GetCurrencyCode(), it.GetUnitPrice().GetAmountMinor()),
		}
	}
	return order{
		ID: o.GetId(), CustomerID: o.GetCustomerId(), Items: items,
		Total:        toMoney(o.GetTotal().GetCurrencyCode(), o.GetTotal().GetAmountMinor()),
		Status:       enumName(o.GetStatus(), "ORDER_STATUS_"),
		CancelReason: o.GetCancelReason(),
		CreatedAt:    o.GetCreateTime().AsTime(),
		UpdatedAt:    o.GetUpdateTime().AsTime(),
	}
}

func toProduct(it *inventoryv1.StockItem) product {
	return product{
		SKU: it.GetSku(), Name: it.GetName(),
		UnitPrice: toMoney(it.GetUnitPrice().GetCurrencyCode(), it.GetUnitPrice().GetAmountMinor()),
		Available: it.GetAvailable(),
	}
}

func toNotification(n *notificationv1.Notification) notification {
	return notification{
		ID: n.GetId(), OrderID: n.GetOrderId(),
		Kind:          enumName(n.GetKind(), "NOTIFICATION_KIND_"),
		Channel:       enumName(n.GetChannel(), "NOTIFICATION_CHANNEL_"),
		Subject:       n.GetSubject(),
		Body:          n.GetBody(),
		Status:        enumName(n.GetStatus(), "NOTIFICATION_STATUS_"),
		FailureReason: n.GetFailureReason(),
		CreatedAt:     n.GetCreateTime().AsTime(),
		UpdatedAt:     n.GetUpdateTime().AsTime(),
	}
}
