package shared

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/mail"
	"github.com/gofabrik/fabrik/query"
	"github.com/gofabrik/fabrik/session"
	_ "modernc.org/sqlite"
)

//fabrik:provider
func NewDB(cfg *DatabaseConfig) (*sql.DB, func(), error) {
	db, err := sql.Open("sqlite", "file:"+cfg.Path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	return db, func() { db.Close() }, nil
}

//fabrik:provider
func NewQueries(db *sql.DB) (*query.DB, error) {
	return query.New(db, query.DialectSQLite)
}

//fabrik:provider
func NewJobStore(db *sql.DB) (jobs.Store, error) {
	// Migrations create the jobs schema before schedule reconciliation.
	return jobs.NewSQLiteStore(db, jobs.SQLiteOptions{AutoCreate: false})
}

// NewJobsConfig configures the generated jobs manager.
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

//fabrik:provider
func NewCrossOrigin(cfg *CrossOriginConfig) (*http.CrossOriginProtection, error) {
	p := http.NewCrossOriginProtection()
	for _, origin := range cfg.TrustedOrigins {
		if err := p.AddTrustedOrigin(origin); err != nil {
			return nil, err
		}
	}
	return p, nil
}

//fabrik:provider
func NewServer(cfg *HTTPConfig) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

//fabrik:provider
func JobsWorker(cfg *JobsConfig) jobs.RuntimeConfig {
	return jobs.RuntimeConfig{
		Worker:       jobs.WorkerConfig{Concurrency: cfg.Concurrency, ShutdownTimeout: cfg.ShutdownTimeout.Duration()},
		RunScheduler: true,
	}
}

// Mailer is the app's delivery seam; mailer.kind selects the transport.
//
//fabrik:provider:select mailer.kind
type Mailer = mail.Transport

//fabrik:provider case=log
func NewLogMailer() *mail.Log { return &mail.Log{} }

//fabrik:provider case=smtp
func NewSMTPMailer(cfg *MailerConfig) *mail.SMTP {
	return cfg.smtp()
}

func (c MailerConfig) smtp() *mail.SMTP {
	return &mail.SMTP{
		Addr:     c.Addr,
		Username: c.Username,
		Password: c.Password,
		TLSMode:  mail.TLSMode(c.TLSMode),
	}
}
