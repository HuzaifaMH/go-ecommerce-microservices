// Command inventory is the entrypoint for the inventory-service.
package main

import (
	"fmt"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
)

func main() {
	// Wiring is added in a later phase.
	fmt.Println("inventory-service", version.String())
}
