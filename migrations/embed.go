package migrations

import "embed"

// Files contains the versioned database migrations.
//
//go:embed *.sql
var Files embed.FS
