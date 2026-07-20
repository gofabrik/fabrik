package dbtest

// Live Postgres coverage runs when TEST_POSTGRES_DSN is set:
//
//	TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/testdb?sslmode=disable' go test ./...

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/query"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/lib/pq"
)

// pgx and lib/pq expose SQLState without core driver imports.
func TestRealDriverErrorTypesClassify(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{&pgconn.PgError{Code: "23505"}, query.ErrUnique},
		{&pgconn.PgError{Code: "23503"}, query.ErrForeignKey},
		{&pgconn.PgError{Code: "23514"}, query.ErrCheck},
		{&pq.Error{Code: "23505"}, query.ErrUnique},
		{&pq.Error{Code: "23503"}, query.ErrForeignKey},
		{&pq.Error{Code: "23514"}, query.ErrCheck},
	}
	for _, c := range cases {
		wrapped := fmt.Errorf("exec: %w", c.err)
		probe := wrapped
		got := classifyProbe(t, probe)
		if !errors.Is(got, c.want) {
			t.Errorf("classify(%T %v) = %v, want %v", c.err, c.err, got, c.want)
		}
	}
}

type errExec struct{ err error }

func (e *errExec) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, e.err
}

func (e *errExec) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, e.err
}

func classifyProbe(t *testing.T, err error) error {
	t.Helper()
	type row struct{ Name string }
	_, got := query.Insert(context.Background(), &errExec{err: err}, query.DialectPostgres, "t", row{Name: "x"})
	return got
}

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
	schema := fmt.Sprintf("querytest_%d", time.Now().UnixNano())
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
		// #nosec G104 -- test database Close is cleanup
		db.Close() //nolint:errcheck // test database Close is cleanup
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		// #nosec G104 -- test admin connection Close is cleanup
		admin.Close() //nolint:errcheck // test admin connection Close is cleanup
	})
	return db
}

func TestPostgres_WriteHelpersLive(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	d := query.DialectPostgres

	if _, err := db.Exec(`CREATE TABLE items (
		id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		name    TEXT NOT NULL UNIQUE,
		created TIMESTAMPTZ NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}

	type item struct {
		ID      int64
		Name    string
		Created time.Time
	}
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	type itemInsert struct {
		Name    string
		Created time.Time
	}
	if _, err := query.Insert(ctx, db, d, "items", itemInsert{Name: "a", Created: created}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := query.InsertMany(ctx, db, d, "items", []itemInsert{
		{Name: "b", Created: created}, {Name: "c", Created: created},
	}); err != nil {
		t.Fatalf("insert many: %v", err)
	}

	type returned struct{ ID int64 }
	row, err := query.One[returned](ctx, db,
		"INSERT INTO items (name, created) VALUES ($1, $2) RETURNING id", "d", created)
	if err != nil || row.ID == 0 {
		t.Fatalf("returning: %v, id=%d", err, row.ID)
	}

	items, err := query.All[item](ctx, db, "SELECT * FROM items WHERE created = $1 ORDER BY id", created)
	if err != nil || len(items) != 4 {
		t.Fatalf("time-bound read: %v, %d rows", err, len(items))
	}
	if !items[0].Created.Equal(created) {
		t.Fatalf("created round trip: %v != %v", items[0].Created, created)
	}

	n, err := query.Update(ctx, db, d, "items", "name = ?", itemInsert{Name: "a2", Created: created}, "a")
	if err != nil || n != 1 {
		t.Fatalf("update: %v, n=%d", err, n)
	}
	n, err = query.Delete(ctx, db, d, "items", "name = ? OR name = ?", "b", "c")
	if err != nil || n != 2 {
		t.Fatalf("delete: %v, n=%d", err, n)
	}

	if _, err := query.Insert(ctx, db, d, "items", itemInsert{Name: "a2", Created: created}); !errors.Is(err, query.ErrUnique) {
		t.Fatalf("duplicate: %v, want ErrUnique", err)
	}
}

// query.JSON must bind as JSONB, not bytea.
func TestPostgres_JSONBRoundTrip(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE docs (
		id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		meta JSONB NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}

	type doc struct {
		ID   int64
		Meta query.JSON[map[string]string]
	}
	type docInsert struct {
		Meta query.JSON[map[string]string]
	}
	if _, err := query.Insert(ctx, db, query.DialectPostgres, "docs", docInsert{
		Meta: query.JSON[map[string]string]{V: map[string]string{"k": "v"}},
	}); err != nil {
		t.Fatalf("insert into JSONB: %v", err)
	}
	got, err := query.One[doc](ctx, db, "SELECT * FROM docs LIMIT 1")
	if err != nil || got.Meta.V["k"] != "v" {
		t.Fatalf("JSONB round trip: %v, %+v", err, got.Meta.V)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM docs WHERE meta ? 'k'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("JSONB operator over stored value: %v, n=%d", err, n)
	}
}
