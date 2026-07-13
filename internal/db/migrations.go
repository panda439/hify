// Package db embeds the golang-migrate migration files into the binary so
// `hify migrate` can apply schema changes against a fresh MySQL instance
// with no separate migration tool needed at deploy time.
package db

import "embed"

//go:embed migrations/*.sql
var MigrationsFS embed.FS
