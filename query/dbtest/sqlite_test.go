package dbtest

// Driver-backed SQLite behavior.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/query"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// #nosec G104 -- test database Close is cleanup
		db.Close() //nolint:errcheck // test database Close is cleanup
	})
	return db
}

type user struct {
	ID      int64
	Email   string
	Bio     *string
	Joined  time.Time
	Deleted *time.Time
	Meta    query.JSON[map[string]string]
	Ignored string `db:"-"`
}

func setup(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE users (
		id      INTEGER PRIMARY KEY,
		email   TEXT NOT NULL UNIQUE,
		bio     TEXT,
		joined  TEXT NOT NULL,
		deleted TEXT,
		meta    TEXT NOT NULL,
		CHECK (email <> '')
	)`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoundTrip(t *testing.T) {
	db := openDB(t)
	setup(t, db)
	ctx := context.Background()

	joined := time.Date(2026, 7, 11, 10, 30, 0, 123456789, time.UTC)
	id, err := query.Insert(ctx, db, query.DialectSQLite, "users", user{
		Email:  "a@example.com",
		Joined: joined,
		Meta:   query.JSON[map[string]string]{V: map[string]string{"k": "v"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}

	got, err := query.One[user](ctx, db, "SELECT * FROM users WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@example.com" || !got.Joined.Equal(joined) || got.Meta.V["k"] != "v" || got.Bio != nil {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Deleted != nil {
		t.Fatalf("NULL time should scan to nil, got %v", got.Deleted)
	}

	deleted := joined.Add(time.Hour)
	if _, err := query.Update(ctx, db, query.DialectSQLite, "users", "id = ?",
		struct{ Deleted *time.Time }{Deleted: &deleted}, id); err != nil {
		t.Fatal(err)
	}
	got, err = query.One[user](ctx, db, "SELECT * FROM users WHERE id = ?", id)
	if err != nil || got.Deleted == nil || !got.Deleted.Equal(deleted) {
		t.Fatalf("nullable time round trip: %v, %+v", err, got.Deleted)
	}

	same, err := query.All[user](ctx, db, "SELECT * FROM users WHERE joined = ?", joined)
	if err != nil || len(same) != 1 {
		t.Fatalf("time-bound read: %v, %d rows", err, len(same))
	}

	if _, err := query.One[user](ctx, db, "SELECT * FROM users WHERE id = ?", 999); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestWriteHelpersAgainstSQLite(t *testing.T) {
	db := openDB(t)
	setup(t, db)
	ctx := context.Background()
	d := query.DialectSQLite

	rows := []user{
		{Email: "a@example.com", Joined: time.Now(), Meta: query.JSON[map[string]string]{}},
		{Email: "b@example.com", Joined: time.Now(), Meta: query.JSON[map[string]string]{}},
	}
	if err := query.InsertMany(ctx, db, d, "users", rows); err != nil {
		t.Fatal(err)
	}

	type patch struct {
		Bio *string
	}
	bio := "hello"
	n, err := query.Update(ctx, db, d, "users", "email = ?", patch{Bio: &bio}, "a@example.com")
	if err != nil || n != 1 {
		t.Fatalf("update: %v, n=%d", err, n)
	}

	n, err = query.Delete(ctx, db, d, "users", "email = ?", "b@example.com")
	if err != nil || n != 1 {
		t.Fatalf("delete: %v, n=%d", err, n)
	}

	left, err := query.All[user](ctx, db, "SELECT * FROM users")
	if err != nil || len(left) != 1 || left[0].Bio == nil || *left[0].Bio != "hello" {
		t.Fatalf("final state: %v, %+v", err, left)
	}
}

func TestRealConstraintErrorsClassify(t *testing.T) {
	db := openDB(t)
	setup(t, db)
	ctx := context.Background()
	d := query.DialectSQLite

	u := user{Email: "a@example.com", Joined: time.Now(), Meta: query.JSON[map[string]string]{}}
	if _, err := query.Insert(ctx, db, d, "users", u); err != nil {
		t.Fatal(err)
	}
	if _, err := query.Insert(ctx, db, d, "users", u); !errors.Is(err, query.ErrUnique) {
		t.Fatalf("duplicate insert: %v, want ErrUnique", err)
	}

	empty := user{Email: "", Joined: time.Now(), Meta: query.JSON[map[string]string]{}}
	if _, err := query.Insert(ctx, db, d, "users", empty); !errors.Is(err, query.ErrCheck) {
		t.Fatalf("check violation: %v, want ErrCheck", err)
	}

	if _, err := db.Exec(`CREATE TABLE posts (
		id      INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id)
	)`); err != nil {
		t.Fatal(err)
	}
	type post struct {
		ID     int64
		UserID int64
	}
	if _, err := query.Insert(ctx, db, d, "posts", post{UserID: 999}); !errors.Is(err, query.ErrForeignKey) {
		t.Fatalf("fk violation: %v, want ErrForeignKey", err)
	}
}

func TestTxAgainstSQLite(t *testing.T) {
	db := openDB(t)
	setup(t, db)
	ctx := context.Background()
	d := query.DialectSQLite

	sentinel := errors.New("boom")
	err := query.Tx(ctx, db, func(tx *sql.Tx) error {
		if _, err := query.Insert(ctx, tx, d, "users", user{Email: "x@example.com", Joined: time.Now(), Meta: query.JSON[map[string]string]{}}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	rows, err := query.All[user](ctx, db, "SELECT * FROM users")
	if err != nil || len(rows) != 0 {
		t.Fatalf("rollback failed: %v, %d rows", err, len(rows))
	}
}

// Duplicate result columns fail before the first row is scanned.
func TestDuplicateColumnsRejectedOnEmptyResult(t *testing.T) {
	db := openDB(t)
	setup(t, db)
	ctx := context.Background()

	type row struct{ ID int64 }
	_, err := query.All[row](ctx, db, "SELECT id, id FROM users")
	if err == nil || !strings.Contains(err.Error(), "appears at positions") {
		t.Fatalf("All over duplicate columns, zero rows: %v, want ambiguity error", err)
	}
	_, err = query.One[row](ctx, db, "SELECT id, id FROM users")
	if err == nil || !strings.Contains(err.Error(), "appears at positions") {
		t.Fatalf("One over duplicate columns, zero rows: %v, want ambiguity error", err)
	}
}

func TestAnonymousStructDiagnosticLabel(t *testing.T) {
	db := openDB(t)
	setup(t, db)
	_, err := query.One[struct{ ID int64 }](context.Background(), db, "SELECT id, id FROM users")
	if err == nil || !strings.Contains(err.Error(), "struct {") || strings.Contains(err.Error(), "scan into :") {
		t.Fatalf("anonymous label: %v", err)
	}
}

// Deferred constraints fail at Commit, not Exec.
func TestTx_DeferredConstraintClassifiesAtCommit(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := db.Exec(`CREATE TABLE parents (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE children (
		id        INTEGER PRIMARY KEY,
		parent_id INTEGER NOT NULL REFERENCES parents(id) DEFERRABLE INITIALLY DEFERRED
	)`); err != nil {
		t.Fatal(err)
	}

	type child struct {
		ID       int64
		ParentID int64
	}
	err := query.Tx(ctx, db, func(tx *sql.Tx) error {
		_, err := query.Insert(ctx, tx, query.DialectSQLite, "children", child{ParentID: 999})
		return err
	})
	if !errors.Is(err, query.ErrForeignKey) {
		t.Fatalf("deferred FK at commit: %v, want ErrForeignKey", err)
	}
}
