// Package sorovault is the module root. It exists to embed assets that are
// meant to stay visible at the top of the repository — currently just the
// SQL migrations, which operators also run directly with the golang-migrate
// CLI, so burying them inside internal/ would make them harder to find.
package sorovault

import "embed"

// Migrations holds the registry's schema migrations, in the layout
// golang-migrate's iofs source expects.
//
//go:embed migrations/*.sql migrations/sqlite/*.sql
var Migrations embed.FS
