// Command order is the entrypoint for the order-service.
package main

import (
	"fmt"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
)

func main() {
	// Wiring is added in a later phase.
	fmt.Println("order-service", version.String())
}
