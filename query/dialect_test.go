package query

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRebindPostgres(t *testing.T) {
	cases := []struct{ in, want string }{
		{"INSERT INTO t (a, b) VALUES (?, ?)", "INSERT INTO t (a, b) VALUES ($1, $2)"},
		{"UPDATE t SET a = ?, b = ? WHERE id = ?", "UPDATE t SET a = $1, b = $2 WHERE id = $3"},
		{`UPDATE t SET "has? mark" = ? WHERE id = ?`, `UPDATE t SET "has? mark" = $1 WHERE id = $2`},
		{`UPDATE t SET a = ? WHERE b = 'lit?eral' AND c = '$1'`, `UPDATE t SET a = $1 WHERE b = 'lit?eral' AND c = '$1'`},
		{`UPDATE t SET a = ? WHERE b = 'it''s ?'`, `UPDATE t SET a = $1 WHERE b = 'it''s ?'`},
		{`UPDATE t SET "a""?b" = ?`, `UPDATE t SET "a""?b" = $1`},
		{`WHERE a = 'unterminated ?`, `WHERE a = 'unterminated ?`},
	}
	for _, c := range cases {
		if got := rebindPostgres(c.in); got != c.want {
			t.Errorf("rebindPostgres(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCheckWhereGuards(t *testing.T) {
	reject := []struct{ name, where, needle string }{
		{"empty", "", "empty where"},
		{"whitespace", "   \t", "empty where"},
		{"dollar one", "id = $1", "$1"},
		{"dollar twelve", "id = $12", "$12"},
		{"line comment", "id = ? -- trailing", "-- comment"},
		{"block comment", "id = ? /* note */", "/* comment"},
		{"dollar quote", "body = $$ ? $$", "dollar-quote"},
		{"tagged dollar quote", "body = $json$ ? $json$ AND id = ?", "$json$"},
		{"tagged dollar quote underscore", "body = $q_1$ ? $q_1$", "$q_1$"},
		{"escape string", `note = E'it\'? inside' AND id = ?`, "escape-string"},
		{"escape string lowercase", "x = e'a'", "escape-string"},
		{"escape string at start", "E'x' = ?", "escape-string"},
		{"jsonb exists operator", "meta ? 'admin'", "string literal"},
		{"numbered placeholder", "id = ?1", "?1"},
		{"numbered placeholder long", "id = ?12 AND b = ?", "?12"},
		{"jsonb exists operator bound rhs", "meta ? ?", "JSONB"},
		{"jsonb exists parenthesized literal", "meta ? ('admin')", "parenthesized"},
		{"jsonb exists parenthesized placeholder", "meta ? (?)", "parenthesized"},
		{"jsonb exists parenthesized call", "meta ? (lower(?))", "parenthesized"},
		{"jsonb exists bound rhs across newline", "meta ?\n?", "JSONB"},
		{"jsonb exists across newline", "meta ?\n'admin'", "string literal"},
		{"jsonb exists-any operator", "meta ?| array['a','b']", "JSONB ?|"},
		{"jsonb exists-all operator", "meta ?& array['a','b']", "JSONB ?&"},
		{"unterminated single", "name = 'abc", "unterminated ' quote"},
		{"unterminated double", `"col = ?`, `unterminated " quote`},
		{"trailing escaped quote", "name = 'ab''", "unterminated ' quote"},
	}
	for _, c := range reject {
		if err := checkWhere("query.Update", c.where); err == nil || !strings.Contains(err.Error(), c.needle) {
			t.Errorf("%s: checkWhere(%q) = %v, want error containing %q", c.name, c.where, err, c.needle)
		}
	}

	accept := []string{
		"id = ?",
		"a = ? AND b = ?",
		"1 = 1",
		"note = '$1'",               // quoted placeholder is data
		"note = '--'",               // quoted comment marker is data
		"note = '/*'",               // quoted comment marker is data
		"note = '$$'",               // quoted dollar marker is data
		"note = '$json$'",           // quoted tagged delimiter is data
		"cost$total = ?",            // $ between identifier chars is not a tag ambiguity here
		"name = ?||'sfx'",           // string concat after a placeholder is not ?|
		"a = ? AND b = ?",           // keyword after a placeholder stays legal
		"CAST(? AS int) = ?",        // parens before ?, cast around it - legal
		"x = ?::text",               // postgres cast directly on a placeholder - legal
		"id IN (?, ?)",              // parens after IN, placeholders inside - legal
		"x = ANY (?)",               // parens after a keyword, not after ?
		"note = 'E'",                // E inside quotes is data
		"name LIKE'%a%' AND id = ?", // keyword tail E + quote is not an E-string
		"e_col = 'y'",               // identifier starting with e, quote after space
		`"weird--name" = ?`,         // quoted identifier with marker
		"price$ = ?",                // lone $ is a valid identifier character
	}
	for _, w := range accept {
		if err := checkWhere("query.Update", w); err != nil {
			t.Errorf("checkWhere(%q) = %v, want nil", w, err)
		}
	}
}

// Every write helper rebinds generated SQL for Postgres.
func TestWriteHelpers_PostgresRebinding(t *testing.T) {
	ctx := context.Background()
	type row struct {
		ID   int64
		Name string
	}

	e := &fakeExecutor{}
	if _, err := Insert(ctx, e, DialectPostgres, "t", row{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if got := e.queries[0]; got != "INSERT INTO t (name) VALUES ($1)" {
		t.Errorf("Insert = %q", got)
	}

	e = &fakeExecutor{}
	if err := InsertMany(ctx, e, DialectPostgres, "t", []row{{Name: "a"}, {Name: "b"}}); err != nil {
		t.Fatal(err)
	}
	if got := e.queries[0]; got != "INSERT INTO t (name) VALUES ($1), ($2)" {
		t.Errorf("InsertMany = %q", got)
	}

	e = &fakeExecutor{}
	if _, err := Update(ctx, e, DialectPostgres, "t", "id = ?", row{Name: "a"}, 7); err != nil {
		t.Fatal(err)
	}
	if got := e.queries[0]; got != "UPDATE t SET id = $1, name = $2 WHERE id = $3" {
		t.Errorf("Update = %q", got)
	}

	e = &fakeExecutor{}
	if _, err := Delete(ctx, e, DialectPostgres, "t", "id = ? OR name = ?", 7, "a"); err != nil {
		t.Fatal(err)
	}
	if got := e.queries[0]; got != "DELETE FROM t WHERE id = $1 OR name = $2" {
		t.Errorf("Delete = %q", got)
	}

	type pkOnly struct{ ID int64 }
	e = &fakeExecutor{}
	if _, err := Insert(ctx, e, DialectPostgres, "t", pkOnly{}); err != nil {
		t.Fatal(err)
	}
	if got := e.queries[0]; got != "INSERT INTO t DEFAULT VALUES" {
		t.Errorf("Insert default values = %q", got)
	}

	type quoted struct {
		V string `db:"\"has? mark\""`
	}
	e = &fakeExecutor{}
	if _, err := Insert(ctx, e, DialectPostgres, "t", quoted{V: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := e.queries[0]; got != `INSERT INTO t ("has? mark") VALUES ($1)` {
		t.Errorf("Insert quoted = %q", got)
	}
}

func TestWriteHelpers_UnknownDialect(t *testing.T) {
	ctx := context.Background()
	type row struct{ Name string }
	e := &fakeExecutor{}
	if _, err := Insert(ctx, e, Dialect(99), "t", row{Name: "a"}); err == nil || !strings.Contains(err.Error(), "unknown dialect") {
		t.Fatalf("Insert with Dialect(99): %v, want unknown-dialect error", err)
	}
	if len(e.queries) != 0 {
		t.Fatal("unknown dialect must fail before executing")
	}
}

// A local SQLState carrier keeps core classifier tests driver-free.
type fakeSQLState struct{ code string }

func (e *fakeSQLState) Error() string    { return "pg error " + e.code }
func (e *fakeSQLState) SQLState() string { return e.code }

func TestBuiltinClassifiers(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{&fakeSQLState{code: "23505"}, ErrUnique},
		{&fakeSQLState{code: "23503"}, ErrForeignKey},
		{&fakeSQLState{code: "23514"}, ErrCheck},
		{fmt.Errorf("constraint failed: UNIQUE constraint failed: t.name (2067)"), ErrUnique},
		{fmt.Errorf("FOREIGN KEY constraint failed (787)"), ErrForeignKey},
		{fmt.Errorf("CHECK constraint failed: t (275)"), ErrCheck},
		{fmt.Errorf("UNIQUE constraint failed: users.email"), ErrUnique},
	}
	for _, c := range cases {
		if got := classify(c.err); !errors.Is(got, c.want) {
			t.Errorf("classify(%v) = %v, want %v", c.err, got, c.want)
		}
		if got := classify(c.err); !errors.Is(got, c.err) {
			t.Errorf("classify(%v) lost the original error", c.err)
		}
	}
	plain := fmt.Errorf("some other failure")
	if got := classify(plain); got != plain {
		t.Errorf("unrelated error changed: %v", got)
	}
}

// Registered classifiers run before built-ins.
func TestRegisteredClassifierRunsBeforeBuiltins(t *testing.T) {
	saveClassifiers(t)
	custom := errors.New("custom sentinel")
	RegisterClassifier(func(err error) error {
		if strings.Contains(err.Error(), "override me") {
			return custom
		}
		return nil
	})
	err := classify(fmt.Errorf("override me: UNIQUE constraint failed: t.c"))
	if !errors.Is(err, custom) {
		t.Fatalf("registered classifier should win: %v", err)
	}
	if errors.Is(err, ErrUnique) {
		t.Fatal("built-in should not have run after the registered sentinel")
	}

	err = classify(fmt.Errorf("UNIQUE constraint failed: t.c"))
	if !errors.Is(err, ErrUnique) {
		t.Fatalf("nil-returning classifier must defer to built-ins: %v", err)
	}
}

// Bad generic usage fails before any query runs.
func TestReadsRejectNonStructTBeforeQuerying(t *testing.T) {
	ctx := context.Background()
	e := &fakeExecutor{}
	if _, err := All[int](ctx, e, "SELECT 1"); err == nil || !strings.Contains(err.Error(), "must be a struct") {
		t.Fatalf("All[int]: %v, want struct error", err)
	}
	if _, err := One[int](ctx, e, "SELECT 1"); err == nil || !strings.Contains(err.Error(), "must be a struct") {
		t.Fatalf("One[int]: %v, want struct error", err)
	}
	if len(e.queries) != 0 {
		t.Fatal("invalid T must fail before the query executes")
	}

	type dup struct {
		Name  string
		Name2 string `db:"name"`
	}
	if _, err := All[dup](ctx, e, "SELECT 1"); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("All[dup]: %v, want ErrDuplicateColumn before querying", err)
	}
	type nested struct {
		Inner struct{ X int }
	}
	if _, err := One[nested](ctx, e, "SELECT 1"); !errors.Is(err, ErrUnsupportedFieldType) {
		t.Fatalf("One[nested]: %v, want ErrUnsupportedFieldType before querying", err)
	}
	if len(e.queries) != 0 {
		t.Fatal("bad field maps must fail before the query executes")
	}
}

// Valuer-only and Scanner-only types are direction-specific.
type valuerOnly struct{ v string }

func (x valuerOnly) Value() (driver.Value, error) { return x.v, nil }

type scannerOnly struct{}

func (x *scannerOnly) Scan(src any) error { return nil }

// Reads need Scanner; writes need Valuer.
func TestColumnStructDirectionValidation(t *testing.T) {
	ctx := context.Background()
	e := &fakeExecutor{}

	type readRow struct{ V valuerOnly }
	if _, err := All[readRow](ctx, e, "SELECT 1"); !errors.Is(err, ErrUnsupportedFieldType) || !strings.Contains(err.Error(), "not sql.Scanner") {
		t.Fatalf("All over Valuer-only field: %v", err)
	}

	type writeRow struct{ V scannerOnly }
	if _, err := Insert(ctx, e, DialectSQLite, "t", writeRow{}); !errors.Is(err, ErrUnsupportedFieldType) || !strings.Contains(err.Error(), "does not implement driver.Valuer") {
		t.Fatalf("Insert of Scanner-only field: %v", err)
	}
	if _, err := Update(ctx, e, DialectSQLite, "t", "id = ?", writeRow{}, 1); !errors.Is(err, ErrUnsupportedFieldType) {
		t.Fatalf("Update of Scanner-only field: %v", err)
	}

	if _, err := Insert(ctx, e, DialectSQLite, "t", readRow{}); err != nil {
		t.Fatalf("Insert of Valuer-only field should pass validation: %v", err)
	}
	type scanOK struct{ V scannerOnly }
	if err := checkRowType[scanOK](); err != nil {
		t.Fatalf("read validation of Scanner-only field should pass: %v", err)
	}
	type ptrScanOK struct{ V *scannerOnly }
	if err := checkRowType[ptrScanOK](); err != nil {
		t.Fatalf("read validation of *Scanner field should pass: %v", err)
	}
	type ptrValRow struct{ V *valuerOnly }
	if err := checkRowType[ptrValRow](); !errors.Is(err, ErrUnsupportedFieldType) {
		t.Fatalf("read validation of *Valuer-only field: %v, want rejection", err)
	}
	if len(e.queries) != 1 {
		t.Fatalf("only the one valid write should reach the executor, got %d", len(e.queries))
	}
}

// InsertMany validates type and dialect even for empty input.
func TestInsertMany_ValidatesBeforeEmptyNoop(t *testing.T) {
	ctx := context.Background()
	e := &fakeExecutor{}
	if err := InsertMany(ctx, e, DialectSQLite, "t", []int{}); err == nil || !strings.Contains(err.Error(), "must be a struct") {
		t.Fatalf("empty []int: %v, want element-type error", err)
	}
	type dup struct {
		Name  string
		Name2 string `db:"name"`
	}
	if err := InsertMany(ctx, e, DialectSQLite, "t", []dup{}); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("empty []dup: %v, want ErrDuplicateColumn", err)
	}
	type row struct{ Name string }
	if err := InsertMany(ctx, e, Dialect(99), "t", []row{}); err == nil || !strings.Contains(err.Error(), "unknown dialect") {
		t.Fatalf("empty rows, bad dialect: %v, want unknown-dialect error", err)
	}
	if err := InsertMany(ctx, e, DialectSQLite, "t", []row{}); err != nil {
		t.Fatalf("valid empty slice stays a no-op: %v", err)
	}
	if len(e.queries) != 0 {
		t.Fatal("nothing should have executed")
	}
}

// Time fields support only time.Time and *time.Time.
func TestDeepTimePointersRejected(t *testing.T) {
	ctx := context.Background()
	e := &fakeExecutor{}
	type deep struct{ At **time.Time }
	if err := checkRowType[deep](); !errors.Is(err, ErrUnsupportedFieldType) || !strings.Contains(err.Error(), "time.Time or *time.Time only") {
		t.Fatalf("read validation: %v", err)
	}
	if _, err := Insert(ctx, e, DialectSQLite, "t", deep{}); !errors.Is(err, ErrUnsupportedFieldType) {
		t.Fatalf("write validation: %v", err)
	}
	if len(e.queries) != 0 {
		t.Fatal("nothing should execute")
	}
}
