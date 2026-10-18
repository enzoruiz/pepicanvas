// Package migrations embeds the versioned PostgreSQL schema migrations.
package migrations

import "embed"

// FS holds every goose SQL migration, applied in version order.
//
//go:embed *.sql
var FS embed.FS
