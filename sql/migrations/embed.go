package migrations

import "embed"

// Files contains the SQL migrations used by Goose in integration tests.
//
//go:embed *.sql
var Files embed.FS
