package shared

import (
	"context"
	"database/sql"
	"log/slog"
	"os"

	"github.com/gofabrik/fabrik/migrations"
)

//fabrik:hook setup
func InitLogger(l *Log) error {
	var level slog.Level
	if err := level.UnmarshalText([]byte(l.Level)); err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return nil
}

//fabrik:hook start
func MigrateDB(ctx context.Context, db *sql.DB, src migrations.Sources) error {
	// Dialect is composition metadata, not a dependency: the app is SQLite,
	// so it is written at the call site rather than injected.
	return src.Migrate(ctx, db, migrations.DialectSQLite)
}
