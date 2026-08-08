package shared

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gofabrik/fabrik/cache"
	"github.com/gofabrik/fabrik/flash"
	"github.com/gofabrik/fabrik/jobs"
	"github.com/gofabrik/fabrik/mail"
	mailtemplates "github.com/gofabrik/fabrik/mail/templates"
	"github.com/gofabrik/fabrik/query"
	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/storage"
	"github.com/gofabrik/fabrik/web"
	_ "modernc.org/sqlite"
)

//fabrik:provider name=database
func NewDB(cfg *DatabaseConfig) (*sql.DB, func() error, error) {
	db, err := sql.Open("sqlite", "file:"+cfg.Path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, err
	}
	return db, db.Close, nil
}

//fabrik:inject db name=database
//fabrik:provider
func NewQueries(db *sql.DB) (*query.DB, error) {
	return query.New(db, query.DialectSQLite)
}

//fabrik:inject db name=database
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

//fabrik:inject db name=database
//fabrik:provider
func NewSession(db *sql.DB, c *SessionConfig) (*session.Manager[Session], error) {
	store, err := session.NewSQLiteStore(db, session.SQLiteOptions{})
	if err != nil {
		return nil, err
	}
	return session.New[Session](session.Config{
		Store:          store,
		Token:          session.Cookie{Name: "demo_session", HttpOnly: true, Secure: c.CookieSecure, SameSite: http.SameSiteLaxMode},
		AbsoluteExpiry: 24 * time.Hour,
		IdleExpiry:     time.Hour,
	})
}

//fabrik:provider
func NewFlash(m *session.Manager[Session]) (*flash.Flash, error) {
	return flash.New(m)
}

//fabrik:inject db name=database
//fabrik:provider
func NewCacheStore(db *sql.DB) (cache.Store, func() error, error) {
	// Schema creation belongs to migration 0005_cache.sql.
	store, err := cache.NewSQLiteStore(db, cache.SQLiteOptions{AutoCreate: false})
	if err != nil {
		return nil, nil, err
	}
	// Reads leave expired rows in place; sweep them out periodically.
	// Cleanup joins the goroutine so no sweep can outlive the database.
	ctx, cancel := context.WithCancel(context.Background())
	ticker := time.NewTicker(time.Hour)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-ticker.C:
				if _, err := store.Sweep(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("cache sweep failed", "error", err)
				}
			case <-ctx.Done():
				return
			}
		}
	})
	return store, func() error {
		ticker.Stop()
		cancel()
		wg.Wait()
		return nil
	}, nil
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

//fabrik:provider case=dev
func NewDevMailer() *mail.Dev { return &mail.Dev{} }

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

//fabrik:provider
func NewEmailTemplates() (*mailtemplates.Renderer, error) {
	return mailtemplates.Load(MailTemplates, "mail")
}

//fabrik:provider
func NewRatelimitStore() (*ratelimit.MemoryStore, func() error) {
	store := ratelimit.NewMemoryStore()
	ticker := time.NewTicker(time.Minute)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				_ = store.Sweep(context.Background(), time.Now())
			case <-done:
				return
			}
		}
	}()
	return store, func() error {
		ticker.Stop()
		close(done)
		return nil
	}
}

//fabrik:provider
func NewStorage(cfg *StorageConfig) (storage.Storage, func() error, error) {
	if err := os.MkdirAll(cfg.Path, 0o755); err != nil { // #nosec G301 -- demo storage uses conventional owner-writable directory permissions
		return nil, nil, err
	}
	local, err := storage.NewLocal(cfg.Path)
	if err != nil {
		return nil, nil, err
	}
	return local, local.Close, nil
}

// NewTemplateFuncs supplies the app's static template helpers.
//
//fabrik:provider
func NewTemplateFuncs() web.FuncMap {
	return web.FuncMap{
		"shout":       strings.ToUpper,
		"humanizeAge": humanizeAge,
	}
}

func humanizeAge(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// NewTemplateRequestFuncs declares the request-scoped values templates may
// read: the typed session and the pending flash messages.
//
//fabrik:provider
func NewTemplateRequestFuncs(sessions *session.Manager[Session], fl *flash.Flash) web.RequestFuncs {
	return web.RequestFuncs{
		"session": func(r *http.Request) any {
			return func() (Session, error) { return sessions.Get(r.Context()) }
		},
		"flashes": func(r *http.Request) any {
			ctx := r.Context()
			var taken []flash.Message
			var consumed bool
			return func() ([]flash.Message, error) {
				// Cache the result so repeated calls consume flashes only once.
				if !consumed {
					msgs, err := fl.Take(ctx)
					if err != nil {
						return nil, err
					}
					taken, consumed = msgs, true
				}
				return taken, nil
			}
		},
	}
}
