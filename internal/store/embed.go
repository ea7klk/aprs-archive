package store

import _ "embed"

// schemaSQL is embedded so the collector binary carries its schema. The web
// process never applies it.
//
//go:embed schema.sql
var schemaSQL string
