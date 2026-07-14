package shared

import (
	"context"
	"database/sql"
	"log/slog"
	"os"

	"github.com/gofabrik/fabrik/jobs"
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

//fabrik:hook prepare
func MigrateDB(ctx context.Context, db *sql.DB, src migrations.Sources) error {
	return src.Migrate(ctx, db, migrations.DialectSQLite)
}

//fabrik:hook start
func StartJobs(ctx context.Context, mgr *jobs.Manager) error {
	w, err := jobs.NewWorker(mgr, jobs.WorkerConfig{Concurrency: 2})
	if err != nil {
		return err
	}
	// Sync declared schedules to the store synchronously, so a failure
	// aborts startup instead of being lost in the scheduler goroutine.
	if err := mgr.ReconcileSchedules(ctx); err != nil {
		return err
	}
	go func() { _ = w.Start(ctx) }()
	go func() { _ = mgr.StartScheduler(ctx) }()
	return nil
}
