package migrations

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

var (
	errApply = errors.New("apply failed")
	errClose = errors.New("close failed")
)

var testDriverSeq atomic.Int64

// emptyDriver lets Migrate reach Apply and Close without existing migrations.
type emptyDriver struct{}

func (emptyDriver) Open(string) (sqldriver.Conn, error) { return emptyConn{}, nil }

type emptyConn struct{}

func (emptyConn) Prepare(string) (sqldriver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (emptyConn) Begin() (sqldriver.Tx, error) { return nil, errors.New("begin unsupported") }
func (emptyConn) Close() error                 { return nil }

func (emptyConn) ExecContext(context.Context, string, []sqldriver.NamedValue) (sqldriver.Result, error) {
	return sqldriver.ResultNoRows, nil
}

func (emptyConn) QueryContext(_ context.Context, q string, _ []sqldriver.NamedValue) (sqldriver.Rows, error) {
	if strings.Contains(q, "schema_migrations") {
		return emptyRows{}, nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

type emptyRows struct{}

func (emptyRows) Columns() []string {
	return []string{"stream", "version", "name", "checksum", "applied_at"}
}
func (emptyRows) Close() error                 { return nil }
func (emptyRows) Next([]sqldriver.Value) error { return io.EOF }

// failingDriver's session fails Apply and Close with distinct errors.
type failingDriver struct{ db *sql.DB }

func (failingDriver) Placeholder(i int) string                           { return fmt.Sprintf("$%d", i) }
func (failingDriver) SchemaSQL() string                                  { return "CREATE TABLE IF NOT EXISTS schema_migrations ()" }
func (failingDriver) TableExists(context.Context, Querier) (bool, error) { return true, nil }
func (d failingDriver) OpenSession(ctx context.Context, db *sql.DB) (Session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	return failingSession{c: conn}, nil
}

type failingSession struct{ c *sql.Conn }

func (s failingSession) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.c.ExecContext(ctx, q, args...)
}
func (s failingSession) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.c.QueryContext(ctx, q, args...)
}
func (failingSession) Apply(context.Context, string, Migration, string) error { return errApply }
func (s failingSession) Close() error {
	_ = s.c.Close()
	return errClose
}

func TestIdentLenBound(t *testing.T) {
	cases := []struct {
		label   string
		s       string
		wantErr bool
	}{
		{"190 bytes", strings.Repeat("a", 190), false},
		{"191 bytes (at bound)", strings.Repeat("a", 191), false},
		{"192 bytes (over bound)", strings.Repeat("a", 192), true},
		// Sixty-four three-byte runes exceed the byte limit despite the low rune count.
		{"multibyte 192 bytes 64 runes", strings.Repeat("中", 64), true},
		{"multibyte 63 bytes 21 runes", strings.Repeat("中", 21), false},
	}
	for _, c := range cases {
		if err := checkIdentLen(c.s); (err != nil) != c.wantErr {
			t.Errorf("%s: checkIdentLen(%d bytes) err = %v, wantErr %v", c.label, len(c.s), err, c.wantErr)
		}
	}
}

func TestLoadStreamsRejectsOverlongStream(t *testing.T) {
	over := strings.Repeat("a", 192)
	srcs := Sources{{
		Stream: over,
		FS:     fstest.MapFS{"0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1")}},
	}}
	_, err := loadStreams(srcs)
	if !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("loadStreams overlong stream: got %v, want ErrInvalidSource", err)
	}
}

func TestLoadStreamsMigrationRejectsOverlongName(t *testing.T) {
	over := strings.Repeat("a", 192)
	srcs := Sources{{
		Stream: "s",
		FS:     fstest.MapFS{"0001_" + over + ".sql": &fstest.MapFile{Data: []byte("SELECT 1")}},
	}}
	_, err := loadStreams(srcs)
	if !errors.Is(err, ErrInvalidFilename) {
		t.Fatalf("loadStreams overlong migration name: got %v, want ErrInvalidFilename", err)
	}
}

// A close failure may indicate a held advisory lock and must remain visible with the migration failure.
func TestMigrateJoinsApplyAndCloseErrors(t *testing.T) {
	name := fmt.Sprintf("migrations-empty-%d", testDriverSeq.Add(1))
	sql.Register(name, emptyDriver{})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck

	fsys := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte("CREATE TABLE t (id INT)")},
	}

	err = Migrate(context.Background(), db, failingDriver{db: db}, fsys)
	if !errors.Is(err, errApply) {
		t.Fatalf("Migrate error = %v, want the apply failure reachable via errors.Is", err)
	}
	if !errors.Is(err, errClose) {
		t.Fatalf("Migrate error = %v, want the close failure reachable via errors.Is", err)
	}
}
