// Package migrations embeds the SQL schema migrations (goose format).
// Files are applied in order of their numeric prefix and are never edited
// after they are merged; a change to the schema is a new file.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
