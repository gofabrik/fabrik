package query

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSnakeCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ID", "id"},
		{"UserID", "user_id"},
		{"URL", "url"},
		{"HTTPRequest", "http_request"},
		{"alreadyLower", "already_lower"},
		{"IDList", "id_list"},
		{"X", "x"},
		{"", ""},
		{"PostID", "post_id"},
		{"CreatedAt", "created_at"},
		{"š", "š"},
		{"café", "café"},
	}
	for _, c := range cases {
		got := snakeCase(c.in)
		if got != c.want {
			t.Errorf("snakeCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFieldMap_BuiltOncePerType(t *testing.T) {
	type User struct {
		ID    int64
		Email string
		Name  string `db:"display_name"`
	}
	fm1, err := getFieldMap(reflect.TypeOf(User{}))
	if err != nil {
		t.Fatalf("getFieldMap: unexpected error %v", err)
	}
	fm2, _ := getFieldMap(reflect.TypeOf(User{}))
	if fm1 != fm2 {
		t.Errorf("fieldMap not cached: got distinct pointers %p vs %p", fm1, fm2)
	}
	if fm1.pkIndex != 0 || fm1.fields[0].column != "id" {
		t.Errorf("PK not detected: pkIndex=%d, first column=%q", fm1.pkIndex, fm1.fields[0].column)
	}
	if got := fm1.colToField["display_name"]; got != 2 {
		t.Errorf("db tag override not honored: colToField[display_name]=%d, want 2", got)
	}
}

func TestFieldMap_SkipsUnexported(t *testing.T) {
	type withPrivate struct {
		ID     int64
		Public string
		hidden string //nolint:unused // intentional unexported field for test
	}
	fm, err := getFieldMap(reflect.TypeOf(withPrivate{}))
	if err != nil {
		t.Fatalf("getFieldMap: unexpected error %v", err)
	}
	if len(fm.fields) != 2 {
		t.Errorf("expected 2 exported fields, got %d", len(fm.fields))
	}
}

// Duplicate derived columns are struct-definition errors.
func TestFieldMap_DuplicateColumnError(t *testing.T) {
	type T struct {
		Alpha string `db:"x"`
		Beta  string `db:"x"`
	}
	_, err := getFieldMap(reflect.TypeOf(T{}))
	if !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("error = %v, want ErrDuplicateColumn", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate column") {
		t.Errorf("error %q does not say %q", msg, "duplicate column")
	}
	if !strings.Contains(msg, "Alpha") || !strings.Contains(msg, "Beta") {
		t.Errorf("error %q does not name both colliding fields Alpha and Beta", msg)
	}
	if !strings.Contains(msg, `"x"`) {
		t.Errorf("error %q does not name the collided column", msg)
	}
}

func TestFieldMap_TagCollidesWithDerivedName(t *testing.T) {
	type T struct {
		UserID int64  // derives to "user_id"
		Other  string `db:"user_id"` // explicit override collides
	}
	if _, err := getFieldMap(reflect.TypeOf(T{})); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("error = %v, want ErrDuplicateColumn when db: tag collides with derived name", err)
	}
}

// Scan matching is case-insensitive, so case-only columns collide.
func TestFieldMap_CaseOnlyDuplicateColumnError(t *testing.T) {
	type T struct {
		A string `db:"Name"`
		B string `db:"name"`
	}
	if _, err := getFieldMap(reflect.TypeOf(T{})); !errors.Is(err, ErrDuplicateColumn) {
		t.Fatalf("error = %v, want ErrDuplicateColumn for a case-only column collision", err)
	}
}

// `db:"-"` excludes a field from writes and scans.
func TestFieldMap_DashTagSkipsField(t *testing.T) {
	type T struct {
		ID       int64
		Email    string
		Internal string `db:"-"`
	}
	fm, err := getFieldMap(reflect.TypeOf(T{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(fm.fields) != 2 {
		t.Errorf("fields = %d, want 2 (Internal skipped)", len(fm.fields))
	}
	if _, ok := fm.colToField["internal"]; ok {
		t.Error("skipped field 'Internal' present in colToField")
	}
	if _, ok := fm.colToField["-"]; ok {
		t.Error("'-' present as a column key")
	}

	exec := &fakeExecutor{}
	if _, err := Insert(context.Background(), exec, DialectSQLite, "t", T{Email: "a@x", Internal: "secret"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exec.queries[0], "internal") {
		t.Errorf("INSERT names the skipped column: %q", exec.queries[0])
	}
	for _, a := range exec.args[0] {
		if a == "secret" {
			t.Errorf("INSERT bound the skipped field's value: args=%v", exec.args[0])
		}
	}
}

// moneyValuerScanner is a user-defined struct column type.
type moneyValuerScanner struct{ cents int64 }

func (m moneyValuerScanner) Value() (driver.Value, error) { return m.cents, nil }
func (m *moneyValuerScanner) Scan(v any) error {
	if n, ok := v.(int64); ok {
		m.cents = n
	}
	return nil
}

// Plain nested structs are rejected; column-type structs are allowed.
func TestFieldMap_RejectsUnsupportedStructFields(t *testing.T) {
	type Address struct{ City, Zip string }
	type Base struct {
		ID        int64
		CreatedAt time.Time
	}

	t.Run("embedded plain struct", func(t *testing.T) {
		type User struct {
			Base
			Email string
		}
		if _, err := getFieldMap(reflect.TypeOf(User{})); !errors.Is(err, ErrUnsupportedFieldType) {
			t.Fatalf("err = %v, want ErrUnsupportedFieldType for embedded struct", err)
		}
	})
	t.Run("named nested plain struct", func(t *testing.T) {
		type User struct {
			Email string
			Addr  Address
		}
		if _, err := getFieldMap(reflect.TypeOf(User{})); !errors.Is(err, ErrUnsupportedFieldType) {
			t.Fatalf("err = %v, want ErrUnsupportedFieldType for nested struct", err)
		}
	})
	t.Run("pointer to plain struct", func(t *testing.T) {
		type User struct {
			Email string
			Addr  *Address
		}
		if _, err := getFieldMap(reflect.TypeOf(User{})); !errors.Is(err, ErrUnsupportedFieldType) {
			t.Fatalf("err = %v, want ErrUnsupportedFieldType for *struct", err)
		}
	})
	t.Run("db:- skips an embedded struct", func(t *testing.T) {
		type User struct {
			Base  `db:"-"`
			Email string
		}
		fm, err := getFieldMap(reflect.TypeOf(User{}))
		if err != nil {
			t.Fatalf(`db:"-" embedded struct should be skipped, got %v`, err)
		}
		if len(fm.fields) != 1 || fm.fields[0].column != "email" {
			t.Errorf("fields = %+v, want only email", fm.fields)
		}
	})
	t.Run("accepts column-type structs", func(t *testing.T) {
		type Row struct {
			ID      int64
			Name    sql.Null[string]
			Meta    JSON[map[string]any]
			At      time.Time
			Balance moneyValuerScanner
		}
		if _, err := getFieldMap(reflect.TypeOf(Row{})); err != nil {
			t.Fatalf("column-type structs must be accepted, got %v", err)
		}
	})
}

// PK values without an exact int64 form return 0.
func TestPkAsInt64(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want int64
	}{
		{"int64", int64(42), 42},
		{"int32", int32(-7), -7},
		{"uint32 max", uint32(math.MaxUint32), int64(math.MaxUint32)},
		{"uint64 in range", uint64(123), 123},
		{"uint64 == MaxInt64", uint64(math.MaxInt64), math.MaxInt64},
		{"uint64 over MaxInt64", uint64(math.MaxUint64), 0},
		{"string PK", "uuid-abc", 0},
	}
	for _, c := range cases {
		if got := pkAsInt64(reflect.ValueOf(c.v)); got != c.want {
			t.Errorf("%s: pkAsInt64(%v) = %d, want %d", c.name, c.v, got, c.want)
		}
	}
}

// Reused JSON scanners must not leak values between scans.
func TestJSON_ScanZeroesBeforeUnmarshal(t *testing.T) {
	var j JSON[map[string]int]
	if err := j.Scan([]byte(`{"a": 1}`)); err != nil {
		t.Fatal(err)
	}
	if err := j.Scan([]byte(`{"b": 2}`)); err != nil {
		t.Fatal(err)
	}
	if _, stale := j.V["a"]; stale || j.V["b"] != 2 {
		t.Fatalf("stale state after rescan: %v", j.V)
	}

	type twoFields struct{ A, B int }
	var js JSON[twoFields]
	if err := js.Scan([]byte(`{"A": 1, "B": 1}`)); err != nil {
		t.Fatal(err)
	}
	if err := js.Scan([]byte(`{"B": 2}`)); err != nil {
		t.Fatal(err)
	}
	if js.V.A != 0 || js.V.B != 2 {
		t.Fatalf("absent field kept stale value: %+v", js.V)
	}
}

func TestJSON_ValueMarshalError(t *testing.T) {
	j := JSON[chan int]{V: make(chan int)}
	if _, err := j.Value(); err == nil {
		t.Fatal("JSON.Value() with unmarshalable payload = nil error, want an error (no panic)")
	}
}

// InsertMany accepts only slices of structs.
func TestInsertMany_RejectsNonStructElements(t *testing.T) {
	type Row struct {
		ID   int64
		Name string
	}
	t.Run("pointer elements", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, DialectSQLite, "t", []*Row{{Name: "x"}})
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'slice element must be a struct'", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("interface elements", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, DialectSQLite, "t", []any{Row{Name: "x"}})
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'slice element must be a struct'", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
}

// Non-ASCII field names fail identifier validation.
func TestFieldMap_NonASCIIFieldRejected(t *testing.T) {
	type T struct {
		Ł string
	}
	if _, err := getFieldMap(reflect.TypeOf(T{})); !errors.Is(err, ErrInvalidIdentifier) {
		t.Fatalf("error = %v, want ErrInvalidIdentifier for a non-ASCII field name", err)
	}
}

type fakeExecutor struct {
	queries []string
	args    [][]any
	lastID  int64
}

func (e *fakeExecutor) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	e.queries = append(e.queries, q)
	e.args = append(e.args, args)
	return fakeResult{lastID: e.lastID}, nil
}
func (e *fakeExecutor) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, errors.New("fakeExecutor: QueryContext unused")
}
func (e *fakeExecutor) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}

type fakeResult struct{ lastID int64 }

func (r fakeResult) LastInsertId() (int64, error) { return r.lastID, nil }
func (r fakeResult) RowsAffected() (int64, error) { return 0, nil }

// Explicit PKs round-trip even when LastInsertId is unavailable.
func TestInsert_SuppliedPKReturnedNotLastInsertID(t *testing.T) {
	type User struct {
		ID    int64
		Email string
	}
	exec := &fakeExecutor{lastID: 0}
	id, err := Insert(context.Background(), exec, DialectSQLite, "users", User{ID: 42, Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("Insert returned %d, want 42 (caller-supplied PK must round-trip on drivers without LastInsertId)", id)
	}
	if !strings.Contains(exec.queries[0], "(id, email)") {
		t.Errorf("INSERT column list = %q, want includes (id, email)", exec.queries[0])
	}
}

// Zero PKs use the database-assigned id.
func TestInsert_ZeroPKFallsBackToLastInsertID(t *testing.T) {
	type User struct {
		ID    int64
		Email string
	}
	exec := &fakeExecutor{lastID: 99}
	id, err := Insert(context.Background(), exec, DialectSQLite, "users", User{Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 99 {
		t.Errorf("Insert returned %d, want 99 (LastInsertId from fake driver)", id)
	}
	if strings.Contains(exec.queries[0], "id") {
		t.Errorf("INSERT column list unexpectedly includes id: %q", exec.queries[0])
	}
}

// Non-integer PKs cannot be represented by Insert's int64 return.
func TestInsert_NonIntegerPKReturnsZero(t *testing.T) {
	type Event struct {
		ID   string `db:"id"`
		Body string
	}
	exec := &fakeExecutor{lastID: 0}
	id, err := Insert(context.Background(), exec, DialectSQLite, "events", Event{ID: "uuid-abc", Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if id != 0 {
		t.Errorf("Insert with string PK returned %d, want 0", id)
	}
	if !strings.Contains(exec.queries[0], "(id, body)") {
		t.Errorf("INSERT column list = %q, want includes (id, body)", exec.queries[0])
	}
}

// A single all-default row uses INSERT DEFAULT VALUES.
func TestInsert_PKOnlyZero_DefaultValues(t *testing.T) {
	type Row struct{ ID int64 }
	exec := &fakeExecutor{lastID: 7}
	id, err := Insert(context.Background(), exec, DialectSQLite, "t", Row{})
	if err != nil {
		t.Fatal(err)
	}
	if id != 7 {
		t.Errorf("id = %d, want 7 (LastInsertId)", id)
	}
	q := exec.queries[0]
	if !strings.Contains(q, "DEFAULT VALUES") {
		t.Errorf("query = %q, want DEFAULT VALUES form", q)
	}
	if strings.Contains(q, "()") {
		t.Errorf("query = %q, must not emit empty () VALUES ()", q)
	}
}

func TestInsert_PKOnlyNonZero_IncludesColumn(t *testing.T) {
	type Row struct{ ID int64 }
	exec := &fakeExecutor{}
	id, err := Insert(context.Background(), exec, DialectSQLite, "t", Row{ID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if id != 5 {
		t.Errorf("id = %d, want 5 (supplied PK)", id)
	}
	q := exec.queries[0]
	if strings.Contains(q, "DEFAULT VALUES") {
		t.Errorf("query = %q, should insert the supplied id, not DEFAULT VALUES", q)
	}
	if !strings.Contains(q, "(id)") {
		t.Errorf("query = %q, want (id) column list", q)
	}
}

// InsertMany has no portable multi-row all-defaults form.
func TestInsertMany_PKOnlyZero_Errors(t *testing.T) {
	type Row struct{ ID int64 }
	exec := &fakeExecutor{}
	err := InsertMany(context.Background(), exec, DialectSQLite, "t", []Row{{}, {}})
	if !errors.Is(err, ErrNoColumns) {
		t.Fatalf("error = %v, want ErrNoColumns", err)
	}
	if len(exec.queries) != 0 {
		t.Fatalf("executed %d queries, want 0", len(exec.queries))
	}
}

// Empty row structs fail before any query is issued.
func TestWriteHelpers_RejectEmptyStruct(t *testing.T) {
	type empty struct{}
	type onlyPrivate struct {
		secret int //nolint:unused // only unexported fields
	}

	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, DialectSQLite, "users", empty{})
		assertNoColumns(t, exec, err)
	})
	t.Run("InsertMany", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, DialectSQLite, "users", []empty{{}})
		assertNoColumns(t, exec, err)
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, DialectSQLite, "users", "id = ?", empty{}, 1)
		assertNoColumns(t, exec, err)
	})
	t.Run("Update_onlyUnexported", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, DialectSQLite, "users", "id = ?", onlyPrivate{}, 1)
		assertNoColumns(t, exec, err)
	})
}

func assertNoColumns(t *testing.T, exec *fakeExecutor, err error) {
	t.Helper()
	if !errors.Is(err, ErrNoColumns) {
		t.Fatalf("error = %v, want ErrNoColumns", err)
	}
	if len(exec.queries) != 0 {
		t.Fatalf("executed %d queries, want 0: %q", len(exec.queries), exec.queries)
	}
}

// Nil row inputs fail before reflection can panic.
func TestWriteHelpers_NilRow(t *testing.T) {
	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, DialectSQLite, "t", nil)
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want a 'must be a struct' error (no panic)", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, DialectSQLite, "t", "id = ?", nil, 1)
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want a 'must be a struct' error (no panic)", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("InsertMany", func(t *testing.T) {
		exec := &fakeExecutor{}
		err := InsertMany(context.Background(), exec, DialectSQLite, "t", nil)
		if err == nil {
			t.Fatal("err = nil, want an error (no panic)")
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
}

// Write helpers require struct values, not pointers.
func TestWriteHelpers_PointerToStructRejected(t *testing.T) {
	type Row struct {
		ID    int64
		Email string
	}
	t.Run("Insert", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Insert(context.Background(), exec, DialectSQLite, "t", &Row{Email: "x"})
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'must be a struct' for *struct", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
	t.Run("Update", func(t *testing.T) {
		exec := &fakeExecutor{}
		_, err := Update(context.Background(), exec, DialectSQLite, "t", "id = ?", &Row{Email: "x"}, 1)
		if err == nil || !strings.Contains(err.Error(), "must be a struct") {
			t.Fatalf("err = %v, want 'must be a struct' for *struct", err)
		}
		if len(exec.queries) != 0 {
			t.Errorf("executed %d queries, want 0", len(exec.queries))
		}
	})
}

// Update writes every field, including zero-valued id fields.
func TestUpdate_WritesZeroPKInSet(t *testing.T) {
	type Row struct {
		ID    int64
		Email string
	}
	exec := &fakeExecutor{}
	if _, err := Update(context.Background(), exec, DialectSQLite, "t", "email = ?", Row{Email: "x"}, "old@x"); err != nil {
		t.Fatal(err)
	}
	q := exec.queries[0]
	if !strings.Contains(q, "id = ?") {
		t.Errorf("UPDATE query = %q, want it to set id = ? (Update writes every column, incl. a zero PK)", q)
	}
	if exec.args[0][0] != int64(0) {
		t.Errorf("first SET arg = %v (%T), want int64(0) — zero PK written verbatim", exec.args[0][0], exec.args[0][0])
	}
}

// Zero time values normalize consistently in value and pointer form.
func TestArgOf_TimeNormalization(t *testing.T) {
	zero := time.Time{}
	stamp := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	want := stamp.Format(time.RFC3339Nano)

	if got := argOf(zero); got != "" {
		t.Errorf("argOf(zero time.Time) = %v, want \"\"", got)
	}
	if got := argOf(&zero); got != "" {
		t.Errorf("argOf(*time.Time at zero) = %v, want \"\" (must match value zero)", got)
	}
	var nilPtr *time.Time
	if got := argOf(nilPtr); got != nil {
		t.Errorf("argOf(nil *time.Time) = %v, want nil (SQL NULL)", got)
	}
	if got := argOf(stamp); got != want {
		t.Errorf("argOf(stamp) = %v, want %q", got, want)
	}
	if got := argOf(&stamp); got != want {
		t.Errorf("argOf(&stamp) = %v, want %q (must match value)", got, want)
	}
}

// Exec-based helpers normalize time arguments before binding.
func TestBindArgNormalization_ExecEntryPoints(t *testing.T) {
	ctx := context.Background()
	stamp := time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC)
	want := stamp.UTC().Format(time.RFC3339Nano)

	type Row struct {
		ID        int64
		CreatedAt time.Time
	}
	type Plain struct {
		ID   int64
		Name string
	}

	t.Run("Insert row value", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Insert(ctx, exec, DialectSQLite, "t", Row{CreatedAt: stamp}); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("InsertMany row value", func(t *testing.T) {
		exec := &fakeExecutor{}
		if err := InsertMany(ctx, exec, DialectSQLite, "t", []Row{{CreatedAt: stamp}}); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("Update SET value", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Update(ctx, exec, DialectSQLite, "t", "id = ?", Row{CreatedAt: stamp}, 1); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("Update where arg", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Update(ctx, exec, DialectSQLite, "t", "created_at = ?", Plain{Name: "x"}, stamp); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
	t.Run("Delete where arg", func(t *testing.T) {
		exec := &fakeExecutor{}
		if _, err := Delete(ctx, exec, DialectSQLite, "t", "created_at < ?", stamp); err != nil {
			t.Fatal(err)
		}
		assertNormalized(t, exec.args[0], want)
	})
}

// assertNormalized rejects raw time.Time bind args.
func assertNormalized(t *testing.T, args []any, want string) {
	t.Helper()
	found := false
	for _, a := range args {
		if _, isTime := a.(time.Time); isTime {
			t.Fatalf("un-normalized time.Time reached the driver in args %v", args)
		}
		if s, ok := a.(string); ok && s == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("normalized time %q not found in bind args %v", want, args)
	}
}

// Classifier registration is safe under concurrent classification.
func TestClassifierRegistry_ConcurrentRegisterAndClassify(t *testing.T) {
	saveClassifiers(t)

	const iterations = 2000

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			RegisterClassifier(func(error) error { return nil })
		}
	}()

	go func() {
		defer wg.Done()
		exec := &erroringExecutor{err: errors.New("driver: boom")}
		type Row struct {
			ID int64
			X  string
		}
		for i := 0; i < iterations; i++ {
			_, _ = Insert(context.Background(), exec, DialectSQLite, "t", Row{X: "x"})
		}
	}()

	wg.Wait()
}

// saveClassifiers isolates registry-mutating tests.
func saveClassifiers(t *testing.T) {
	t.Helper()
	classifiersMu.Lock()
	saved := append([]Classifier(nil), classifiers...)
	classifiers = nil
	classifiersMu.Unlock()
	t.Cleanup(func() {
		classifiersMu.Lock()
		classifiers = saved
		classifiersMu.Unlock()
	})
}

// Registered classifiers are not deduplicated.
func TestClassifier_RegisteredTwiceRunsTwice(t *testing.T) {
	saveClassifiers(t)

	var calls int
	c := func(error) error { calls++; return nil }
	RegisterClassifier(c)
	RegisterClassifier(c)

	_ = classify(errors.New("boom"))
	if calls != 2 {
		t.Errorf("classifier ran %d times, want 2 (the library does not deduplicate)", calls)
	}
}

// Concurrent registration must not hide existing classifiers.
func TestClassifierRegistry_SentinelReturnedUnderConcurrentRegister(t *testing.T) {
	saveClassifiers(t)

	marker := errors.New("driver: boom")
	RegisterClassifier(func(err error) error {
		if errors.Is(err, marker) {
			return ErrUnique
		}
		return nil
	})

	const iterations = 2000
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			RegisterClassifier(func(error) error { return nil })
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if got := classify(marker); !errors.Is(got, ErrUnique) {
				t.Errorf("classify returned %v, want it to wrap ErrUnique", got)
				return
			}
		}
	}()

	wg.Wait()
}

type erroringExecutor struct{ err error }

func (e *erroringExecutor) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	return nil, e.err
}
func (e *erroringExecutor) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, e.err
}
func (e *erroringExecutor) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}
