// Package db embeds the golang-migrate migration files into the binary so
// `hify migrate` can apply schema changes against fresh MySQL and PostgreSQL
// instances with no separate migration tool needed at deploy time.
package db

import "embed"

//go:embed migrations/*.sql
var MigrationsFS embed.FS

// PGMigrationsFS holds the PostgreSQL (pgvector chunk store) migrations.
// Each database keeps its own independent schema_migrations table — version
// numbers are never comparable across the two.
//
//go:embed pgmigrations/*.sql
var PGMigrationsFS embed.FS
