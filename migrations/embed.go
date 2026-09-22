// Package migrations embeds the goose SQL migrations so a single binary can migrate
// itself on boot and the same files can be run by the standalone migrate command.
package migrations

import "embed"

// FS holds every .sql migration in this directory, ordered by filename.
//
//go:embed *.sql
var FS embed.FS
