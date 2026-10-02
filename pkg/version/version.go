// Package version exposes build metadata injected at link time via -ldflags.
package version

import "fmt"

// These are overridden at build time:
//
//	go build -ldflags "-X github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version.Version=v1.2.3 \
//	  -X github.com/HuzaifaMH/go-ecommerce-microservices/pkg/version.Commit=abc1234"
var (
	Version = "dev"
	Commit  = "unknown"
)

// String returns a human-readable version string, e.g. "v1.2.3 (abc1234)".
func String() string {
	return fmt.Sprintf("%s (%s)", Version, Commit)
}
