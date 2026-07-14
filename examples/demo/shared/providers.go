package shared

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/query"
	"github.com/gofabrik/fabrik/session"
	_ "modernc.org/sqlite"
)

//fabrik:provider
func NewDB(cfg *Database) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+cfg.Path+"?_pragma=busy_timeout(5000)")
}

//fabrik:provider
func NewQueries(db *sql.DB) (*query.DB, error) {
	return query.New(db, query.DialectSQLite)
}

//fabrik:provider
func NewJobStore(db *sql.DB) (jobs.Store, error) {
	// The jobs schema is created by a migration (0004_jobs.sql), not
	// AutoCreate, so the demo exercises the real lifecycle: migrations run
	// in a prepare hook, then StartJobs reconciles schedules against the
	// schema they created.
	return jobs.NewSQLiteStore(db, jobs.SQLiteOptions{AutoCreate: false})
}

// NewJobsConfig configures the manager the //fabrik:job directive builds.
// The directive emits jobs.New(store, config); store and config both come
// from providers, so this is where the manager is tuned (logger, hooks,
// scheduler group). Drop this provider and the directive falls back to
// jobs.Config{} defaults.
//
//fabrik:provider
func NewJobsConfig() jobs.Config {
	return jobs.Config{Logger: slog.Default()}
}

//fabrik:provider
func NewSession(db *sql.DB) (*session.Manager[Session], error) {
	store, err := session.NewSQLiteStore(db, session.SQLiteOptions{})
	if err != nil {
		return nil, err
	}
	return session.New[Session](session.Config{
		Store:          store,
		Token:          session.Cookie{Name: "demo_session", HttpOnly: true, SameSite: http.SameSiteLaxMode},
		AbsoluteExpiry: 24 * time.Hour,
		IdleExpiry:     time.Hour,
	})
}

//fabrik:provider
func NewFlash(m *session.Manager[Session]) (*flash.Flash, error) {
	return flash.New(m)
}
