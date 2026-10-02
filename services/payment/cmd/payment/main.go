// Command payment is the entrypoint for the payment-service.
package main

import (
	"fmt"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
)

func main() {
	// Wiring is added in a later phase.
	fmt.Println("payment-service", version.String())
}
