package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/query"
)

func newUser(email string) user {
	return user{Email: email, Joined: time.Now(), Meta: query.JSON[map[string]string]{V: map[string]string{}}}
}

// fakeExec cannot start transactions.
type fakeExec struct{}

func (fakeExec) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}
func (fakeExec) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, nil
}

func TestHandle(t *testing.T) {
	db := openDB(t)
	setup(t, db)
	ctx := context.Background()

	q, err := query.New(db, query.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	if q.Dialect() != query.DialectSQLite {
		t.Fatalf("Dialect() = %v", q.Dialect())
	}

	id, err := q.Insert(ctx, "users", newUser("h@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}

	got, err := query.One[user](ctx, q, "SELECT * FROM users WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "h@example.com" {
		t.Fatalf("email = %q", got.Email)
	}

	if err := q.Tx(ctx, func(tq *query.DB) error {
		_, e := tq.Insert(ctx, "users", newUser("tx@example.com"))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	all, err := query.All[user](ctx, q, "SELECT * FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("rows = %d, want 2", len(all))
	}

	boom := errors.New("boom")
	if e := q.Tx(ctx, func(tq *query.DB) error {
		_, _ = tq.Insert(ctx, "users", newUser("rollback@example.com"))
		return boom
	}); !errors.Is(e, boom) {
		t.Fatalf("Tx error = %v, want boom", e)
	}
	if all, _ := query.All[user](ctx, q, "SELECT * FROM users"); len(all) != 2 {
		t.Fatalf("after rollback rows = %d, want 2", len(all))
	}

	fake, err := query.New(fakeExec{}, query.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	if e := fake.Tx(ctx, func(*query.DB) error { return nil }); e == nil {
		t.Fatal("Tx over a non-*sql.DB handle: want error")
	}
}

func TestNewValidates(t *testing.T) {
	db := openDB(t)
	if _, err := query.New(nil, query.DialectSQLite); err == nil {
		t.Fatal("New(nil, ...): want error")
	}
	var typedNil *sql.DB
	if _, err := query.New(typedNil, query.DialectSQLite); err == nil {
		t.Fatal("New(typed-nil *sql.DB, ...): want error")
	}
	if _, err := query.New(db, query.Dialect(99)); err == nil {
		t.Fatal("New(db, invalid dialect): want error")
	}
	q, err := query.New(db, query.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := query.New(q, query.DialectPostgres); err == nil {
		t.Fatal("New over a *query.DB: want error")
	}
}
