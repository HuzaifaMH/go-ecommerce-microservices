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

// InventoryStream carries every inventory command and event.
var InventoryStream = messaging.StreamConfig{
	Name:     "INVENTORY",
	Subjects: []string{"inventory.>"},
	MaxAge:   7 * 24 * time.Hour,
}
