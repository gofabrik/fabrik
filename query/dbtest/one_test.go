package dbtest

// Driver-backed behavior of Scalar, UpdateOne and DeleteOne.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/query"
)

func setupCounters(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE counters (
		id    INTEGER PRIMARY KEY,
		name  TEXT NOT NULL,
		hits  INTEGER NOT NULL DEFAULT 0,
		seen  TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO counters (id, name, hits, seen) VALUES
		(1, 'first', 3, '2026-01-02T03:04:05Z'),
		(2, 'second', 0, NULL)`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestScalar(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	setupCounters(t, db)

	t.Run("int", func(t *testing.T) {
		got, err := query.Scalar[int](ctx, db, `SELECT COUNT(*) FROM counters`)
		if err != nil {
			t.Fatal(err)
		}
		if got != 2 {
			t.Fatalf("count = %d, want 2", got)
		}
	})

	t.Run("string", func(t *testing.T) {
		got, err := query.Scalar[string](ctx, db, `SELECT name FROM counters WHERE id = ?`, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got != "first" {
			t.Fatalf("name = %q, want %q", got, "first")
		}
	})

	t.Run("time matches One", func(t *testing.T) {
		got, err := query.Scalar[time.Time](ctx, db, `SELECT seen FROM counters WHERE id = ?`, 1)
		if err != nil {
			t.Fatal(err)
		}
		if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !got.Equal(want) {
			t.Fatalf("seen = %v, want %v", got, want)
		}
	})

	t.Run("no rows is ErrNotFound", func(t *testing.T) {
		_, err := query.Scalar[int](ctx, db, `SELECT hits FROM counters WHERE id = ?`, 99)
		if !errors.Is(err, query.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("aggregate over nothing still returns a row", func(t *testing.T) {
		got, err := query.Scalar[int](ctx, db, `SELECT COUNT(*) FROM counters WHERE id = ?`, 99)
		if err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Fatalf("count = %d, want 0", got)
		}
	})

	t.Run("more than one column is rejected", func(t *testing.T) {
		_, err := query.Scalar[int](ctx, db, `SELECT id, hits FROM counters`)
		if err == nil {
			t.Fatal("want an error for a two column query")
		}
		if errors.Is(err, query.ErrNotFound) {
			t.Fatalf("err = %v, want a column count error", err)
		}
	})
}

type counterName struct {
	Name string
}

func TestUpdateOne(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	setupCounters(t, db)

	q, err := query.New(db, query.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("one row", func(t *testing.T) {
		if err := q.UpdateOne(ctx, "counters", "id = ?", counterName{Name: "renamed"}, 1); err != nil {
			t.Fatal(err)
		}
		got, err := query.Scalar[string](ctx, db, `SELECT name FROM counters WHERE id = ?`, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got != "renamed" {
			t.Fatalf("name = %q, want %q", got, "renamed")
		}
	})

	t.Run("no row is ErrNotFound", func(t *testing.T) {
		err := q.UpdateOne(ctx, "counters", "id = ?", counterName{Name: "x"}, 99)
		if !errors.Is(err, query.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("more than one row is an error", func(t *testing.T) {
		err := q.UpdateOne(ctx, "counters", "1 = 1", counterName{Name: "all"})
		if err == nil {
			t.Fatal("want an error when two rows match")
		}
		if errors.Is(err, query.ErrNotFound) {
			t.Fatalf("err = %v, want an affected count error", err)
		}
		if !strings.Contains(err.Error(), "affected 2 rows") {
			t.Fatalf("err = %v, want the affected count named", err)
		}
		// The statement is applied even though the count is reported as wrong.
		got, err := query.Scalar[int](ctx, db, `SELECT COUNT(*) FROM counters WHERE name = ?`, "all")
		if err != nil {
			t.Fatal(err)
		}
		if got != 2 {
			t.Fatalf("rows named all = %d, want 2", got)
		}
	})
}

func TestDeleteOne(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	setupCounters(t, db)

	q, err := query.New(db, query.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}

	if err := q.DeleteOne(ctx, "counters", "id = ?", 1); err != nil {
		t.Fatal(err)
	}
	if err := q.DeleteOne(ctx, "counters", "id = ?", 1); !errors.Is(err, query.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	left, err := query.Scalar[int](ctx, db, `SELECT COUNT(*) FROM counters`)
	if err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("rows left = %d, want 1", left)
	}
}

func TestDeleteOneMoreThanOne(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	setupCounters(t, db)

	q, err := query.New(db, query.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}

	err = q.DeleteOne(ctx, "counters", "1 = 1")
	if err == nil {
		t.Fatal("want an error when two rows match")
	}
	if errors.Is(err, query.ErrNotFound) {
		t.Fatalf("err = %v, want an affected count error", err)
	}
	if !strings.Contains(err.Error(), "affected 2 rows") {
		t.Fatalf("err = %v, want the affected count named", err)
	}
	// The statement is applied even though the count is reported as wrong.
	left, err := query.Scalar[int](ctx, db, `SELECT COUNT(*) FROM counters`)
	if err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("rows left = %d, want 0", left)
	}
}

// A bare aggregate produces a row whatever it counted, while a grouped one
// can produce none. Testing both prevents the former from obscuring the latter.
func TestScalarAggregateRowCount(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	setupCounters(t, db)

	t.Run("a bare aggregate over no matches is a zero, not ErrNotFound", func(t *testing.T) {
		got, err := query.Scalar[int](ctx, db, `SELECT COUNT(*) FROM counters WHERE id = ?`, 404)
		if err != nil {
			t.Fatalf("err = %v, want a value", err)
		}
		if got != 0 {
			t.Fatalf("count = %d, want 0", got)
		}
	})

	t.Run("a grouped aggregate over no matches produces no row", func(t *testing.T) {
		_, err := query.Scalar[int](ctx, db,
			`SELECT COUNT(*) FROM counters WHERE id = ? GROUP BY name`, 404)
		if !errors.Is(err, query.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("HAVING can filter the only row away", func(t *testing.T) {
		_, err := query.Scalar[int](ctx, db,
			`SELECT COUNT(*) FROM counters HAVING COUNT(*) > 1000`)
		if !errors.Is(err, query.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}
