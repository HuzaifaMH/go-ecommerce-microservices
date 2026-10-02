// Package inventory is the order service's gRPC client for the inventory
// service. It provides the authoritative unit prices used to price an order.
package inventory

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/HuzaifaMH/go-ecommerce-microservices/gen/ecommerce/inventory/v1"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/app"
	"github.com/HuzaifaMH/go-ecommerce-microservices/services/order/internal/domain"
)

// maxParallel bounds concurrent calls to the inventory service per request.
const maxParallel = 8

var _ app.Catalog = (*Client)(nil)

// Client implements app.Catalog on top of inventory's gRPC API.
type Client struct {
	inv     inventoryv1.InventoryServiceClient
	timeout time.Duration
}

// NewClient returns a Client. Each lookup gets its own deadline of timeout.
func NewClient(conn grpc.ClientConnInterface, timeout time.Duration) *Client {
	return &Client{inv: inventoryv1.NewInventoryServiceClient(conn), timeout: timeout}
}

// Prices fetches the unit price of every SKU concurrently. An unknown SKU is
// reported as *domain.UnknownSKUError; any other failure (inventory down,
// slow, or erroring) wraps app.ErrCatalogUnavailable.
func (c *Client) Prices(ctx context.Context, skus []string) (map[string]domain.Money, error) {
	prices := make([]domain.Money, len(skus))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallel)
	for i, sku := range skus {
		g.Go(func() error {
			ctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()

			resp, err := c.inv.GetStock(ctx, &inventoryv1.GetStockRequest{Sku: sku})
			if status.Code(err) == codes.NotFound {
				return &domain.UnknownSKUError{SKU: sku}
			}
			if err != nil {
				return fmt.Errorf("%w: get stock for %s: %w", app.ErrCatalogUnavailable, sku, err)
			}
			p := resp.GetItem().GetUnitPrice()
			prices[i] = domain.Money{CurrencyCode: p.GetCurrencyCode(), AmountMinor: p.GetAmountMinor()}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	out := make(map[string]domain.Money, len(skus))
	for i, sku := range skus {
		out[sku] = prices[i]
	}
	return out, nil
}
