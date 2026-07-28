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
	return superviseRuntimes(ctx, server.Run, worker.Run)
}

func superviseRuntimes(ctx context.Context, runtimes ...func(context.Context) error) error {
	if len(runtimes) == 0 {
		return nil
	}
	ectx, ecancel := context.WithCancel(ctx)
	defer ecancel()
	errc := make(chan error, len(runtimes))
	for _, run := range runtimes {
		go func() { errc <- run(ectx) }()
	}
	var result error
	for i := range runtimes {
		if e := <-errc; e != nil && !errors.Is(e, context.Canceled) && result == nil {
			result = e
		}
		if i == 0 {
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

// Database maintenance commands.
//
//fabrik:cli:group name=database
var _database struct{}

// Apply pending database migrations.
//
//fabrik:cli:command path="database migrate"
//fabrik:cli:flag name=dry-run short=n type=bool help="Print what would run without applying migrations."
func Migrate(ctx cli.Context, db *sql.DB, src migrations.Sources, dryRun bool) error {
	if dryRun {
		_, err := fmt.Fprintln(ctx.Stdout(), "would apply pending migrations")
		return err
	}
	if err := src.Migrate(ctx, db, migrations.DialectSQLite); err != nil {
		return err
	}
	_, err := fmt.Fprintln(ctx.Stdout(), "migrations applied")
	return err
}
