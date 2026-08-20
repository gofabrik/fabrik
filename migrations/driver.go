package migrations

import (
	"context"
	"database/sql"
	"strings"
)

// Migration contains a file body and its bookkeeping fields.
type Migration struct {
	Version  int64
	Name     string
	Body     string
	Checksum string
}

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Driver provides database-specific migration behavior.
type Driver interface {
	// Placeholder renders the 1-based bind marker.
	Placeholder(i int) string

	// SchemaSQL creates schema_migrations idempotently.
	SchemaSQL() string

	// TableExists reports whether schema_migrations exists without creating it.
	TableExists(ctx context.Context, q Querier) (bool, error)

	// OpenSession acquires the connection and lock for one Migrate call.
	OpenSession(ctx context.Context, db *sql.DB) (Session, error)
}

// Session owns one Migrate call's connection and lock state.
type Session interface {
	Querier

	// Apply executes and records one migration using insertSQL.
	Apply(ctx context.Context, stream string, m Migration, insertSQL string) error

	// Close releases the lock and returns the connection to the pool.
	Close() error
}

func placeholders(d Driver, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = d.Placeholder(i + 1)
	}
	return strings.Join(parts, ", ")
}
