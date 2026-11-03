// Package migrations holds the database schema changes. They're embedded in
// the binary, so each image carries the schema its code expects, and
// `importer-api migrate up` applies them in version order.
//
// New file: make migration NAME=add_something
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
