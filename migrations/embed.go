// Package migrations carries the SQL schema.
//
// The files stay at the repository root where SPEC.md §4 puts them, and are
// embedded so the runtime image needs nothing copied beside the binary.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
