// Package query provides typed reads, struct-derived writes,
// transactions, and constraint error classification over database/sql.
//
// Reads run caller SQL verbatim and always scan into a struct T,
// including single-column results. Writes generate SQL from flat
// structs and use [Dialect] to choose placeholder style.
//
// Field names map to snake_case columns. Use `db:"name"` to override
// and `db:"-"` to skip a field. Nested structs are not flattened:
// fields must be scalars, time.Time, *time.Time, [JSON], or a
// database/sql Valuer or Scanner type accepted for the operation.
//
// Identifiers vs. values: SQL placeholders can bind values but never
// identifiers. Table names, column names, and where fragments must be
// developer-controlled; bind every user value through placeholders.
package query

import (
	"context"
	"database/sql"
)

// Executor is the database/sql subset shared by *sql.DB and *sql.Tx.
type Executor interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
