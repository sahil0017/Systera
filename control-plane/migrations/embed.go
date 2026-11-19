// Package migrations embeds the versioned PostgreSQL schema migrations.
//
// Files are named NNNNNN_description.up.sql / .down.sql. The migrator wraps each file
// together with its schema_migrations bookkeeping row in a single transaction, so files
// must not contain their own BEGIN/COMMIT.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
