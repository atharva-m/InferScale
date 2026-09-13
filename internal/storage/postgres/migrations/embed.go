// Package migrations embeds InferScale's ordered PostgreSQL migrations.
package migrations

import "embed"

// FS contains the forward-only migration files consumed by the migration
// command. Rollback SQL stays checked in for operator reference, but embedding
// both split files would make Goose discover the same numeric version twice.
//
//go:embed *.up.sql
var FS embed.FS
