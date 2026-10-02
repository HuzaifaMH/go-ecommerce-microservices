// Package migrations embeds the order-service SQL migrations (goose format).
package migrations

import "embed"

// FS holds the *.sql migration files at its root.
//
//go:embed *.sql
var FS embed.FS
