// Command notification is the entrypoint for the notification-service.
package main

import (
	"fmt"

	"github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version"
)

func main() {
	// Wiring is added in a later phase.
	fmt.Println("notification-service", version.String())
}
