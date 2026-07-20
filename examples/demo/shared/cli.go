package shared

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gofabrik/fabrik/cli"
	"github.com/gofabrik/fabrik/httpserver"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/migrations"
)

// Print the resolved configuration.
//
//fabrik:cli:command
func Config(ctx cli.Context, cfg *HTTPConfig, db *DatabaseConfig) error {
	if _, err := fmt.Fprintf(ctx.Stdout(), "http addr: %s\n", cfg.Addr); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(ctx.Stdout(), "database:  %s\n", db.Path); err != nil {
		return err
	}
	return nil
}

// Start the HTTP server and background worker.
//
//fabrik:cli:command
func Run(ctx cli.Context, server *httpserver.Server, worker *jobs.Runner) error {
	ectx, ecancel := context.WithCancel(ctx)
	defer ecancel()
	errc := make(chan error, 2)
	go func() { errc <- server.Run(ectx) }()
	go func() { errc <- worker.Run(ectx) }()
	var result error
	for range 2 {
		if e := <-errc; e != nil && !errors.Is(e, context.Canceled) && result == nil {
			result = e
			ecancel()
		}
	}
	return result
}

// Start only the HTTP server.
//
//fabrik:cli:command
func Serve(ctx cli.Context, server *httpserver.Server) error {
	return server.Run(ctx)
}

// Apply pending database migrations.
//
//fabrik:cli:command
func Migrate(ctx cli.Context, db *sql.DB, src migrations.Sources) error {
	if err := src.Migrate(ctx, db, migrations.DialectSQLite); err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout(), "migrations applied")
	return nil
}
