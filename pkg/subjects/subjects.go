// Package subjects is the single source of truth for JetStream stream and
// subject names. It is part of the inter-service contract, next to the
// protobuf messages that travel on these subjects.
//
// Naming: <service>.cmd.<verb> are commands sent to a service,
// <service>.evt.<fact> are facts it publishes.
package subjects

import (
	"time"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/messaging"
)

// Inventory service.
const (
	InventoryCmdReserve = "inventory.cmd.reserve" // ecommerce.inventory.v1.ReserveStock
	InventoryCmdRelease = "inventory.cmd.release" // ecommerce.inventory.v1.ReleaseStock

	InventoryEvtReserved = "inventory.evt.reserved" // ecommerce.inventory.v1.StockReserved
	InventoryEvtRejected = "inventory.evt.rejected" // ecommerce.inventory.v1.StockRejected
	InventoryEvtReleased = "inventory.evt.released" // ecommerce.inventory.v1.StockReleased
)

// Order service. It orchestrates the order saga, so it consumes the
// inventory.evt.* and payment.evt.* replies and publishes the two commands
// above plus its own events.
const (
	OrderEvtConfirmed = "order.evt.confirmed" // ecommerce.order.v1.OrderConfirmed
	OrderEvtCancelled = "order.evt.cancelled" // ecommerce.order.v1.OrderCancelled
)

// OrderStream carries the order lifecycle events.
var OrderStream = messaging.StreamConfig{
	Name:     "ORDER",
	Subjects: []string{"order.>"},
	MaxAge:   7 * 24 * time.Hour,
}

// Payment service.
const (
	PaymentCmdCharge = "payment.cmd.charge" // ecommerce.payment.v1.ChargePayment

	PaymentEvtSucceeded = "payment.evt.succeeded" // ecommerce.payment.v1.PaymentSucceeded
	PaymentEvtFailed    = "payment.evt.failed"    // ecommerce.payment.v1.PaymentFailed
)

// PaymentStream carries every payment command and event.
var PaymentStream = messaging.StreamConfig{
	Name:     "PAYMENT",
	Subjects: []string{"payment.>"},
	MaxAge:   7 * 24 * time.Hour,
}

// InventoryStream carries every inventory command and event.
var InventoryStream = messaging.StreamConfig{
	Name:     "INVENTORY",
	Subjects: []string{"inventory.>"},
	MaxAge:   7 * 24 * time.Hour,
}
