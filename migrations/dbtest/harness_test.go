package dbtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/migrations"
)

// TestApplyRerunDriftOrphan verifies migration history on every backend.
func TestApplyRerunDriftOrphan(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			ctx := context.Background()

			src := fstest.MapFS{
				"0001_users.sql": sqlFile(`CREATE TABLE users (id BIGINT PRIMARY KEY)`),
				"0002_items.sql": sqlFile(`CREATE TABLE items (id BIGINT PRIMARY KEY)`),
			}

			if err := migrations.Migrate(ctx, db, b.driver, src); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if !tableExists(t, db, "users") || !tableExists(t, db, "items") {
				t.Fatalf("expected users and items tables")
			}
			if n := countRows(t, db); n != 2 {
				t.Fatalf("applied rows = %d, want 2", n)
			}

			if err := migrations.Migrate(ctx, db, b.driver, src); err != nil {
				t.Fatalf("rerun: %v", err)
			}
			if n := countRows(t, db); n != 2 {
				t.Fatalf("after rerun rows = %d, want 2", n)
			}

			statuses, err := migrations.Status(ctx, db, b.driver, src)
			if err != nil {
				t.Fatal(err)
			}
			if len(statuses) != 2 || statuses[0].State != migrations.StateApplied || statuses[1].State != migrations.StateApplied {
				t.Fatalf("statuses = %+v, want two applied", statuses)
			}
			if statuses[0].AppliedAt.IsZero() {
				t.Errorf("applied row should carry non-zero AppliedAt (parseTime/DATETIME scan)")
			}

			tampered := fstest.MapFS{
				"0001_users.sql": sqlFile(`CREATE TABLE users (id BIGINT PRIMARY KEY, oops TEXT)`),
				"0002_items.sql": src["0002_items.sql"],
			}
			if err := migrations.Migrate(ctx, db, b.driver, tampered); !errors.Is(err, migrations.ErrDrift) {
				t.Fatalf("tampered: err = %v, want ErrDrift", err)
			}

			truncated := fstest.MapFS{"0001_users.sql": src["0001_users.sql"]}
			if err := migrations.Migrate(ctx, db, b.driver, truncated); !errors.Is(err, migrations.ErrOrphan) {
				t.Fatalf("truncated: err = %v, want ErrOrphan", err)
			}
		})
	}
}

// TestMultiStatementBody verifies multi-statement migrations.
func TestMultiStatementBody(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			src := fstest.MapFS{
				"0001_seed.sql": sqlFile(`CREATE TABLE settings (skey VARCHAR(191) PRIMARY KEY, sval VARCHAR(191) NOT NULL);
INSERT INTO settings (skey, sval) VALUES ('theme', 'dark');`),
			}
			if err := migrations.Migrate(context.Background(), db, b.driver, src); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			var val string
			if err := db.QueryRow(`SELECT sval FROM settings WHERE skey = 'theme'`).Scan(&val); err != nil || val != "dark" {
				t.Fatalf("seeded row: %q, %v", val, err)
			}
		})
	}
}

// TestConcurrentSerializes requires full-call locks to serialize every runner.
func TestConcurrentSerializes(t *testing.T) {
	for _, b := range backends(t) {
		if !b.serializes {
			continue
		}
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			ctx := context.Background()
			src := fstest.MapFS{
				"0001_a.sql": sqlFile(`CREATE TABLE a (id BIGINT PRIMARY KEY)`),
				"0002_b.sql": sqlFile(`CREATE TABLE b (id BIGINT PRIMARY KEY)`),
				"0003_c.sql": sqlFile(`CREATE TABLE c (id BIGINT PRIMARY KEY)`),
			}
			const N = 4
			var wg sync.WaitGroup
			errs := make([]error, N)
			for i := range errs {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					errs[i] = migrations.Migrate(ctx, db, b.driver, src)
				}(i)
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("runner %d: %v (lock should serialize, not race)", i, err)
				}
			}
			if n := countRows(t, db); n != 3 {
				t.Fatalf("rows = %d, want 3", n)
			}
		})
	}
}

// TestConcurrentSQLite allows racing callers to fail but requires complete state.
func TestConcurrentSQLite(t *testing.T) {
	var sqliteB *backend
	for _, b := range backends(t) {
		if b.name == "sqlite" {
			bb := b
			sqliteB = &bb
		}
	}
	db := sqliteB.openDB(t)
	src := fstest.MapFS{
		"0001_a.sql": sqlFile(`CREATE TABLE a (id BIGINT PRIMARY KEY)`),
		"0002_b.sql": sqlFile(`CREATE TABLE b (id BIGINT PRIMARY KEY)`),
		"0003_c.sql": sqlFile(`CREATE TABLE c (id BIGINT PRIMARY KEY)`),
	}
	const N = 4
	results := make(chan error, N)
	for range N {
		go func() { results <- migrations.Migrate(context.Background(), db, sqliteB.driver, src) }()
	}
	succeeded := 0
	for range N {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatal("expected at least one runner to succeed")
	}
	if n := countRows(t, db); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
}

// TestImplicitDDLCommit_MySQL verifies that committed DDL survives a later failure.
func TestImplicitDDLCommit_MySQL(t *testing.T) {
	ran := false
	for _, b := range backends(t) {
		if !b.mysqlFamily {
			continue
		}
		ran = true
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			ctx := context.Background()
			src := fstest.MapFS{
				"0001_mix.sql": sqlFile(`CREATE TABLE leftover (id BIGINT PRIMARY KEY);
CREATE TABLE leftover (id BIGINT PRIMARY KEY);`),
			}
			err := migrations.Migrate(ctx, db, b.driver, src)
			if err == nil {
				t.Fatal("expected the failing second statement to error")
			}
			if n := countRows(t, db); n != 0 {
				t.Errorf("schema_migrations rows = %d, want 0 (insert not committed)", n)
			}
			if !tableExists(t, db, "leftover") {
				t.Errorf("%s: expected 'leftover' table to persist (implicit DDL commit); it did not", b.name)
			} else {
				t.Logf("%s: 'leftover' table persisted after rollback (implicit DDL commit; partial migration state)", b.name)
			}
		})
	}
	if !ran {
		t.Skip("no mysql/mariadb DSN set")
	}
}

// TestImplicitDDLCommit_SQLiteRollsBack requires failed DDL to roll back fully.
func TestImplicitDDLCommit_SQLiteRollsBack(t *testing.T) {
	var sqliteB *backend
	for _, b := range backends(t) {
		if b.name == "sqlite" {
			bb := b
			sqliteB = &bb
		}
	}
	db := sqliteB.openDB(t)
	src := fstest.MapFS{
		"0001_mix.sql": sqlFile(`CREATE TABLE leftover (id BIGINT PRIMARY KEY);
CREATE TABLE leftover (id BIGINT PRIMARY KEY);`),
	}
	err := migrations.Migrate(context.Background(), db, sqliteB.driver, src)
	if err == nil {
		t.Fatal("expected error")
	}
	if tableExists(t, db, "leftover") {
		t.Errorf("sqlite: 'leftover' should have rolled back with the transaction")
	}
}

// TestBadFilename requires validation before database access.
func TestBadFilename(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			src := fstest.MapFS{"not-a-migration.sql": sqlFile(`SELECT 1`)}
			err := migrations.Migrate(context.Background(), db, b.driver, src)
			if !errors.Is(err, migrations.ErrInvalidFilename) {
				t.Fatalf("err = %v, want ErrInvalidFilename", err)
			}
		})
	}
}

func TestNilDriver(t *testing.T) {
	db := openSQLite(t)
	err := migrations.Migrate(context.Background(), db, nil, fstest.MapFS{})
	if err == nil || !strings.Contains(err.Error(), "nil Driver") {
		t.Fatalf("err = %v, want nil Driver error", err)
	}
}

// TestStreamNamesByteExact requires byte-exact stream bookkeeping.
func TestStreamNamesByteExact(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			ctx := context.Background()
			srcs := migrations.Sources{
				{Stream: "Auth", FS: fstest.MapFS{"0001_a.sql": sqlFile(`CREATE TABLE t_upper (id BIGINT PRIMARY KEY)`)}},
				{Stream: "auth", FS: fstest.MapFS{"0001_a.sql": sqlFile(`CREATE TABLE t_lower (id BIGINT PRIMARY KEY)`)}},
				{Stream: "auth ", FS: fstest.MapFS{"0001_a.sql": sqlFile(`CREATE TABLE t_space (id BIGINT PRIMARY KEY)`)}},
			}
			if err := srcs.Migrate(ctx, db, b.driver); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if n := countRows(t, db); n != 3 {
				t.Fatalf("bookkeeping rows = %d, want 3 distinct streams", n)
			}
			if err := srcs.Migrate(ctx, db, b.driver); err != nil {
				t.Fatalf("rerun: %v", err)
			}
			if n := countRows(t, db); n != 3 {
				t.Fatalf("after rerun rows = %d, want 3", n)
			}
		})
	}
}

// TestStreamNameLengthBoundary verifies acceptance at 191 bytes and backend limits above it.
func TestStreamNameLengthBoundary(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			ctx := context.Background()
			fits := incompressible(191)
			srcs := migrations.Sources{{Stream: fits, FS: fstest.MapFS{"0001_a.sql": sqlFile(`CREATE TABLE t_fit (id BIGINT PRIMARY KEY)`)}}}
			if err := srcs.Migrate(ctx, db, b.driver); err != nil {
				t.Fatalf("191-byte stream: %v", err)
			}
			over := "Z" + incompressible(191)
			both := append(migrations.Sources{}, srcs...)
			both = append(both, migrations.Source{Stream: over, FS: fstest.MapFS{"0001_b.sql": sqlFile(`CREATE TABLE t_over (id BIGINT PRIMARY KEY)`)}})
			err := both.Migrate(ctx, db, b.driver)
			if b.mysqlFamily {
				if err == nil {
					t.Fatal("192-byte stream accepted on mysql; it must be rejected")
				}
			} else if err != nil {
				t.Fatalf("192-byte stream on %s: %v", b.name, err)
			}
		})
	}
}

// TestMigrationNameLengthBoundary verifies acceptance at 191 bytes and rejection above it.
func TestMigrationNameLengthBoundary(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			db := b.openDB(t)
			ctx := context.Background()
			fits := incompressible(191)
			srcs := migrations.Sources{{Stream: "s", FS: fstest.MapFS{
				"0001_" + fits + ".sql": sqlFile(`CREATE TABLE t_fit (id BIGINT PRIMARY KEY)`),
			}}}
			if err := srcs.Migrate(ctx, db, b.driver); err != nil {
				t.Fatalf("191-byte name: %v", err)
			}
			over := "Z" + incompressible(191)
			both := append(migrations.Sources{}, srcs...)
			both = append(both, migrations.Source{Stream: "s2", FS: fstest.MapFS{
				"0001_" + over + ".sql": sqlFile(`CREATE TABLE t_over (id BIGINT PRIMARY KEY)`),
			}})
			err := both.Migrate(ctx, db, b.driver)
			if b.mysqlFamily {
				if err == nil {
					t.Fatal("192-byte name accepted on mysql; it must be rejected")
				}
			} else if err != nil {
				t.Fatalf("192-byte name on %s: %v", b.name, err)
			}
		})
	}
}

// incompressible returns deterministic data that resists compression.
func incompressible(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, n)
	state := uint64(0x9E3779B97F4A7C15)
	for i := range out {
		state = state*6364136223846793005 + 1442695040888963407
		out[i] = alphabet[state>>58]
	}
	return string(out)
}
