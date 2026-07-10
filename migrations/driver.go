package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// driver contains the database-specific parts of migration execution.
type driver interface {
	placeholder(i int) string

	schemaSQL() string

	tableExists(ctx context.Context, q querier) (bool, error)

	openSession(ctx context.Context, db *sql.DB) (session, error)
}

// session owns one Migrate call's connection and lock state.
type session interface {
	querier

	apply(ctx context.Context, module string, m migration, insertSQL string) error

	close() error
}

func driverFor(d Dialect) (driver, error) {
	switch d {
	case DialectSQLite:
		return sqliteDriver{}, nil
	case DialectPostgres:
		return pgDriver{}, nil
	}
	return nil, fmt.Errorf("unknown dialect: %s", d)
}

func placeholders(d driver, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = d.placeholder(i + 1)
	}
	return strings.Join(parts, ", ")
}
