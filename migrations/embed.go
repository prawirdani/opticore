// Package migrations embeds the goose SQL migrations applied on start.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
