package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/migrations"
	"time"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrate_EmptySource(t *testing.T) {
	db := openDB(t)
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, fstest.MapFS{}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 applied, got %d", count)
	}
}

func TestMigrate_AppliesInOrder(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"0001_first.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0010_third.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE c (id INTEGER PRIMARY KEY)`)},
		"0002_second.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var v int64
		var n string
		if err := rows.Scan(&v, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d_%s", v, n))
	}
	want := []string{"1_first", "2_second", "10_third"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := db.Exec(fmt.Sprintf(`SELECT * FROM %s`, name)); err != nil {
			t.Errorf("table %s missing: %v", name, err)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src); err != nil {
		t.Fatalf("re-run failed: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 applied row after re-run, got %d", count)
	}
}

func TestMigrate_FailureRollsBack(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"0001_ok.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_bad.sql": &fstest.MapFile{Data: []byte(`THIS IS NOT VALID SQL`)},
		"0003_ok.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE c (id INTEGER PRIMARY KEY)`)},
	}
	err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "2_bad") {
		t.Errorf("error should reference failing migration: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 applied, got %d", count)
	}
	if _, err := db.Exec(`SELECT * FROM a`); err != nil {
		t.Errorf("table a should exist: %v", err)
	}
	if _, err := db.Exec(`SELECT * FROM c`); err == nil {
		t.Errorf("table c should not exist")
	}
}

func TestMigrate_DetectsModifiedMigration(t *testing.T) {
	db := openDB(t)
	original := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, original); err != nil {
		t.Fatal(err)
	}

	modified := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY, x TEXT)`)},
	}
	err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, modified)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !errors.Is(err, migrations.ErrDrift) {
		t.Errorf("expected errors.Is migrations.ErrDrift, got: %v", err)
	}
}

func TestMigrate_ModifiedMigrationBlocksLaterPending(t *testing.T) {
	db := openDB(t)
	original := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, original); err != nil {
		t.Fatal(err)
	}

	tampered := fstest.MapFS{
		"0001_init.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY, oops TEXT)`)},
		"0002_later.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, tampered); err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if _, err := db.Exec(`SELECT * FROM b`); err == nil {
		t.Errorf("table b should not have been created when checksum drift was detected")
	}
}

func TestMigrate_DuplicateVersion(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0001_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
	}
	err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, migrations.ErrDuplicateVersion) {
		t.Errorf("expected errors.Is migrations.ErrDuplicateVersion, got: %v", err)
	}
	var x int
	if err := db.QueryRow(`SELECT 1 FROM schema_migrations`).Scan(&x); err == nil {
		t.Errorf("schema_migrations should not have been created")
	}
}

func TestMigrate_TimestampVersionsAvoidBranchCollisions(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"20260507143022_branch_a_add_users.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE users (id INTEGER PRIMARY KEY)`)},
		"20260507091044_branch_b_add_orders.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE orders (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	want := []string{"branch_b_add_orders", "branch_a_add_users"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMigrate_BlocksWhenSQLiteWriterLockHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := held.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer held.ExecContext(context.Background(), "ROLLBACK")

	src := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY)`)},
	}

	err = migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src)
	if err == nil {
		t.Fatal("expected error when writer lock is held by another conn")
	}
	if _, err := db.Exec(`SELECT * FROM t`); err == nil {
		t.Errorf("table t should not have been created")
	}
}

func TestMigrate_ProceedsAfterLockReleased(t *testing.T) {
	db := openDB(t)

	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(50 * time.Millisecond)
		held.ExecContext(context.Background(), "ROLLBACK")
		held.Close()
	}()
	t.Cleanup(func() { <-released })

	src := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE t (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src); err != nil {
		t.Fatalf("migrations.Migrate should proceed once lock is released: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected migration applied, got %d rows", n)
	}
}

func TestMigrate_RejectsOrphanInDB(t *testing.T) {
	db := openDB(t)
	full := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, full); err != nil {
		t.Fatal(err)
	}

	truncated := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
	}
	err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, truncated)
	if err == nil {
		t.Fatal("expected orphan error")
	}
	if !errors.Is(err, migrations.ErrOrphan) {
		t.Errorf("expected errors.Is migrations.ErrOrphan, got: %v", err)
	}
}

func TestStatus_AppliedPendingDrifted(t *testing.T) {
	db := openDB(t)
	initial := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, initial); err != nil {
		t.Fatal(err)
	}

	inspect := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY, x TEXT)`)},
		"0003_c.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE c (id INTEGER PRIMARY KEY)`)},
	}
	statuses, err := migrations.Status(context.Background(), db, migrations.DialectSQLite, inspect)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(statuses))
	}
	wantStates := []migrations.State{migrations.StateApplied, migrations.StateDrifted, migrations.StatePending}
	for i, s := range statuses {
		if s.State != wantStates[i] {
			t.Errorf("status %d: got %s, want %s", i, s.State, wantStates[i])
		}
	}
	if statuses[0].AppliedAt.IsZero() {
		t.Errorf("applied row should have non-zero AppliedAt")
	}
	if !statuses[2].AppliedAt.IsZero() {
		t.Errorf("pending row should have zero AppliedAt")
	}

	if statuses[0].Checksum == "" || statuses[0].FileChecksum == "" {
		t.Errorf("applied: both checksums should be set, got stored=%q file=%q", statuses[0].Checksum, statuses[0].FileChecksum)
	}
	if statuses[0].Checksum != statuses[0].FileChecksum {
		t.Errorf("applied: Checksum and FileChecksum should match, got stored=%q file=%q", statuses[0].Checksum, statuses[0].FileChecksum)
	}
	if statuses[1].Checksum == "" || statuses[1].FileChecksum == "" {
		t.Errorf("drifted: both checksums should be set, got stored=%q file=%q", statuses[1].Checksum, statuses[1].FileChecksum)
	}
	if statuses[1].Checksum == statuses[1].FileChecksum {
		t.Errorf("drifted: Checksum and FileChecksum should differ, both = %q", statuses[1].Checksum)
	}
	if statuses[2].Checksum != "" {
		t.Errorf("pending: stored Checksum should be empty, got %q", statuses[2].Checksum)
	}
	if statuses[2].FileChecksum == "" {
		t.Error("pending: FileChecksum should be set")
	}
}

func TestStatus_ReportsOrphan(t *testing.T) {
	db := openDB(t)
	full := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, full); err != nil {
		t.Fatal(err)
	}

	older := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
	}
	statuses, err := migrations.Status(context.Background(), db, migrations.DialectSQLite, older)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if statuses[0].State != migrations.StateApplied {
		t.Errorf("0001 should be applied, got %s", statuses[0].State)
	}
	if statuses[1].State != migrations.StateOrphan {
		t.Errorf("0002 should be orphan, got %s", statuses[1].State)
	}
	if statuses[1].Version != 2 || statuses[1].Name != "b" {
		t.Errorf("orphan row: got version=%d name=%q, want 2/b", statuses[1].Version, statuses[1].Name)
	}
	if statuses[1].Checksum == "" {
		t.Error("orphan: stored Checksum should be set")
	}
	if statuses[1].FileChecksum != "" {
		t.Errorf("orphan: FileChecksum should be empty (no source file), got %q", statuses[1].FileChecksum)
	}
}

func TestMigrate_CanceledMigrateLeavesPriorCommitsIntact(t *testing.T) {
	db := openDB(t)

	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, fstest.MapFS{
		"0001_first.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := migrations.Migrate(ctx, db, migrations.DialectSQLite, fstest.MapFS{
		"0001_first.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_second.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
	})
	if err == nil {
		t.Fatal("expected error from migrations.Migrate with canceled ctx")
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 0001 still applied after canceled migrations.Migrate, got %d rows", n)
	}
	if _, err := db.Exec(`SELECT * FROM b`); err == nil {
		t.Error("table b should not exist")
	}
}

// SQLite concurrency is per-migration; racing callers may fail, but the final state is complete.
func TestMigrate_ConcurrentSQLite(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)},
		"0002_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY)`)},
		"0003_c.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE c (id INTEGER PRIMARY KEY)`)},
	}

	const N = 4
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			results <- migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src)
		}()
	}

	var errs []error
	succeeded := 0
	for i := 0; i < N; i++ {
		err := <-results
		if err == nil {
			succeeded++
		} else {
			errs = append(errs, err)
		}
	}
	if succeeded == 0 {
		t.Fatalf("expected at least one migrations.Migrate to succeed; errors: %v", errs)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("expected 3 rows in schema_migrations, got %d", n)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := db.Exec(`SELECT * FROM ` + name); err != nil {
			t.Errorf("table %s missing: %v", name, err)
		}
	}
}

func TestMigrate_BadFilename(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"not-a-migration.sql": &fstest.MapFile{Data: []byte(`SELECT 1`)},
	}
	err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, migrations.ErrInvalidFilename) {
		t.Errorf("expected errors.Is migrations.ErrInvalidFilename, got: %v", err)
	}
}
