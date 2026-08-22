package dbtest

// Postgres integration tests require a real server:
//
//	TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/testdb?sslmode=disable' go test ./...
//
// Each test uses a fresh schema. Without the env var, tests skip.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gofabrik/fabrik/migrations"
	"github.com/gofabrik/fabrik/migrations/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openPG sets search_path in the DSN so every pooled connection sees it.
func openPG(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run Postgres integration tests")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("migtest_%d", time.Now().UnixNano())
	// #nosec G202 -- generated test schema identifier, not user input
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", dsn+sep+"options="+url.QueryEscape("-csearch_path="+schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// #nosec G104 -- test database cleanup cannot affect an earlier assertion
		db.Close() //nolint:errcheck // test database cleanup cannot affect an earlier assertion
		// #nosec G202 -- generated test schema identifier, not user input
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		// #nosec G104 -- test administrator cleanup cannot affect an earlier assertion
		admin.Close() //nolint:errcheck // test administrator cleanup cannot affect an earlier assertion
	})
	return db
}

func TestPostgres_ApplyRerunDriftOrphan(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	src := fstest.MapFS{
		"0001_users.sql": sqlFile(`CREATE TABLE users (id BIGINT PRIMARY KEY)`),
		"0002_items.sql": sqlFile(`CREATE TABLE items (id BIGINT PRIMARY KEY)`),
	}
	if err := migrations.Migrate(ctx, db, postgres.Driver(), src); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Migrate(ctx, db, postgres.Driver(), src); err != nil {
		t.Fatalf("rerun should be idempotent: %v", err)
	}

	statuses, err := migrations.Status(ctx, db, postgres.Driver(), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].State != migrations.StateApplied || statuses[1].State != migrations.StateApplied {
		t.Fatalf("statuses = %+v, want two applied", statuses)
	}

	tampered := fstest.MapFS{
		"0001_users.sql": sqlFile(`CREATE TABLE users (id BIGINT PRIMARY KEY, oops TEXT)`),
		"0002_items.sql": src["0002_items.sql"],
	}
	if err := migrations.Migrate(ctx, db, postgres.Driver(), tampered); !errors.Is(err, migrations.ErrDrift) {
		t.Fatalf("tampered: err = %v, want ErrDrift", err)
	}

	truncated := fstest.MapFS{"0001_users.sql": src["0001_users.sql"]}
	if err := migrations.Migrate(ctx, db, postgres.Driver(), truncated); !errors.Is(err, migrations.ErrOrphan) {
		t.Fatalf("truncated: err = %v, want ErrOrphan", err)
	}
}

func TestPostgres_MultiStatementBody(t *testing.T) {
	db := openPG(t)
	src := fstest.MapFS{
		"0001_seed.sql": sqlFile(`
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO settings (key, value) VALUES ('theme', 'dark');
CREATE INDEX settings_value ON settings (value);
`),
	}
	if err := migrations.Migrate(context.Background(), db, postgres.Driver(), src); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'theme'`).Scan(&value); err != nil || value != "dark" {
		t.Fatalf("seeded row: %q, %v", value, err)
	}
}

func TestPostgres_AdvisoryLockSerializes(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	src := fstest.MapFS{
		"0001_slow.sql": sqlFile(`CREATE TABLE slow (id BIGINT PRIMARY KEY); SELECT pg_sleep(0.5)`),
		"0002_fast.sql": sqlFile(`CREATE TABLE fast (id BIGINT PRIMARY KEY)`),
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = migrations.Migrate(ctx, db, postgres.Driver(), src)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("runner %d: %v (advisory lock should serialize, not race)", i, err)
		}
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("schema_migrations rows = %d, %v; want 2", n, err)
	}
}

// TestPostgres_UnlockFailureEvictsConnection verifies that unlock failure evicts the connection and leaves the pool usable.
func TestPostgres_UnlockFailureEvictsConnection(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	sess, err := postgres.Driver().OpenSession(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var sessPID int
	rows, err := sess.QueryContext(ctx, "SELECT pg_backend_pid()")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() || rows.Scan(&sessPID) != nil {
		t.Fatal("read session backend pid")
	}
	rows.Close() //nolint:errcheck
	if _, err := sess.ExecContext(ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err == nil || !strings.Contains(err.Error(), "not held") {
		t.Fatalf("Close error = %v, want not-held failure", err)
	}

	var nextPID int
	if err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&nextPID); err != nil {
		t.Fatal(err)
	}
	if nextPID == sessPID {
		t.Fatalf("backend pid %d reused after failed unlock; want the session connection evicted", sessPID)
	}

	src := fstest.MapFS{"0001_evict.sql": sqlFile(`CREATE TABLE t_evict (id BIGINT PRIMARY KEY)`)}
	if err := migrations.Migrate(ctx, db, postgres.Driver(), src); err != nil {
		t.Fatalf("Migrate after tainted close: %v", err)
	}
}
