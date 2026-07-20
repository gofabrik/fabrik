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
	if err := migrations.Migrate(ctx, db, migrations.DialectPostgres, src); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Migrate(ctx, db, migrations.DialectPostgres, src); err != nil {
		t.Fatalf("rerun should be idempotent: %v", err)
	}

	statuses, err := migrations.Status(ctx, db, migrations.DialectPostgres, src)
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
	if err := migrations.Migrate(ctx, db, migrations.DialectPostgres, tampered); !errors.Is(err, migrations.ErrDrift) {
		t.Fatalf("tampered: err = %v, want ErrDrift", err)
	}

	truncated := fstest.MapFS{"0001_users.sql": src["0001_users.sql"]}
	if err := migrations.Migrate(ctx, db, migrations.DialectPostgres, truncated); !errors.Is(err, migrations.ErrOrphan) {
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
	if err := migrations.Migrate(context.Background(), db, migrations.DialectPostgres, src); err != nil {
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
			errs[i] = migrations.Migrate(ctx, db, migrations.DialectPostgres, src)
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
