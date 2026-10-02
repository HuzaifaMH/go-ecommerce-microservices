// Command gateway is the entrypoint for the api-gateway.
package main

import (
	"fmt"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
)

func main() {
	// Wiring is added in a later phase.
	fmt.Println("api-gateway", version.String())
}
