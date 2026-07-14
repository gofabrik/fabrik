package shared

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/gofabrik/fabrik/flash"
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
