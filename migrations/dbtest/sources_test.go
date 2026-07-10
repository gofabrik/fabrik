package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gofabrik/fabrik/migrations"
)

func sqlFile(body string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(body)}
}

func TestSourcesMigrate_MultiStream(t *testing.T) {
	db := openDB(t)
	srcs := migrations.Sources{
		{Module: "todos", FS: fstest.MapFS{"0001_todos.sql": sqlFile(`CREATE TABLE todos (id INTEGER PRIMARY KEY)`)}},
		{Module: "auth", FS: fstest.MapFS{"0001_users.sql": sqlFile(`CREATE TABLE users (id INTEGER PRIMARY KEY)`)}},
	}
	if err := srcs.Migrate(context.Background(), db, migrations.DialectSQLite); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT module, version, name FROM schema_migrations ORDER BY module, version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var m, n string
		var v int64
		if err := rows.Scan(&m, &v, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, m+"/"+n)
	}
	if len(got) != 2 || got[0] != "auth/users" || got[1] != "todos/todos" {
		t.Fatalf("bookkeeping rows = %v", got)
	}
	for _, table := range []string{"users", "todos"} {
		if _, err := db.Exec(`SELECT * FROM ` + table); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}

	if err := srcs.Migrate(context.Background(), db, migrations.DialectSQLite); err != nil {
		t.Fatalf("re-run: %v", err)
	}
}

func TestSourcesMigrate_DirSubtree(t *testing.T) {
	db := openDB(t)
	srcs := migrations.Sources{
		{Module: "shared", Dir: "migrations", FS: fstest.MapFS{
			"migrations/0001_a.sql": sqlFile(`CREATE TABLE a (id INTEGER PRIMARY KEY)`),
		}},
	}
	if err := srcs.Migrate(context.Background(), db, migrations.DialectSQLite); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT * FROM a`); err != nil {
		t.Errorf("table a missing: %v", err)
	}
}

func TestSourcesMigrate_FailFastAcrossStreams(t *testing.T) {
	db := openDB(t)
	srcs := migrations.Sources{
		{Module: "a", FS: fstest.MapFS{"0001_ok.sql": sqlFile(`CREATE TABLE t_a (id INTEGER PRIMARY KEY)`)}},
		{Module: "b", FS: fstest.MapFS{"0001_bad.sql": sqlFile(`NOT VALID SQL`)}},
		{Module: "c", FS: fstest.MapFS{"0001_ok.sql": sqlFile(`CREATE TABLE t_c (id INTEGER PRIMARY KEY)`)}},
	}
	err := srcs.Migrate(context.Background(), db, migrations.DialectSQLite)
	if err == nil || !strings.Contains(err.Error(), "b/1_bad") {
		t.Fatalf("err = %v, want failure naming b/1_bad", err)
	}
	if _, err := db.Exec(`SELECT * FROM t_a`); err != nil {
		t.Error("stream a should have applied before the failure")
	}
	if _, err := db.Exec(`SELECT * FROM t_c`); err == nil {
		t.Error("stream c should have been skipped after the failure")
	}
}

func TestSourcesMigrate_DuplicateModule(t *testing.T) {
	db := openDB(t)
	srcs := migrations.Sources{
		{Module: "web", FS: fstest.MapFS{}},
		{Module: "web", FS: fstest.MapFS{}},
	}
	err := srcs.Migrate(context.Background(), db, migrations.DialectSQLite)
	if !errors.Is(err, migrations.ErrDuplicateModule) {
		t.Fatalf("err = %v, want migrations.ErrDuplicateModule", err)
	}
}

func TestSourcesMigrate_InvalidSources(t *testing.T) {
	db := openDB(t)
	cases := []struct {
		name string
		srcs migrations.Sources
	}{
		{"empty", migrations.Sources{}},
		{"nil FS", migrations.Sources{{Module: "web"}}},
		{"absolute dir", migrations.Sources{{Module: "web", FS: fstest.MapFS{}, Dir: "/migrations"}}},
		{"dotdot dir", migrations.Sources{{Module: "web", FS: fstest.MapFS{}, Dir: "../migrations"}}},
		{"bad module", migrations.Sources{{Module: "web//x", FS: fstest.MapFS{}}}},
		{"dot module", migrations.Sources{{Module: ".", FS: fstest.MapFS{}}}},
	}
	for _, tc := range cases {
		if err := tc.srcs.Migrate(context.Background(), db, migrations.DialectSQLite); !errors.Is(err, migrations.ErrInvalidSource) {
			t.Errorf("%s: err = %v, want migrations.ErrInvalidSource", tc.name, err)
		}
	}
	var x int
	if err := db.QueryRow(`SELECT 1 FROM schema_migrations`).Scan(&x); err == nil {
		t.Error("schema_migrations should not exist after validation-only failures")
	}
}

func TestSourcesMigrate_RemovedStreamIsOrphan(t *testing.T) {
	db := openDB(t)
	both := migrations.Sources{
		{Module: "auth", FS: fstest.MapFS{"0001_users.sql": sqlFile(`CREATE TABLE users (id INTEGER PRIMARY KEY)`)}},
		{Module: "todos", FS: fstest.MapFS{"0001_todos.sql": sqlFile(`CREATE TABLE todos (id INTEGER PRIMARY KEY)`)}},
	}
	if err := both.Migrate(context.Background(), db, migrations.DialectSQLite); err != nil {
		t.Fatal(err)
	}
	onlyTodos := both[1:]
	err := onlyTodos.Migrate(context.Background(), db, migrations.DialectSQLite)
	if !errors.Is(err, migrations.ErrOrphan) || !strings.Contains(err.Error(), "auth/1_users") {
		t.Fatalf("err = %v, want orphan naming auth/1_users", err)
	}
}

func TestSourcesMigrate_PerStreamDrift(t *testing.T) {
	db := openDB(t)
	original := migrations.Sources{
		{Module: "auth", FS: fstest.MapFS{"0001_users.sql": sqlFile(`CREATE TABLE users (id INTEGER PRIMARY KEY)`)}},
	}
	if err := original.Migrate(context.Background(), db, migrations.DialectSQLite); err != nil {
		t.Fatal(err)
	}
	tampered := migrations.Sources{
		{Module: "auth", FS: fstest.MapFS{"0001_users.sql": sqlFile(`CREATE TABLE users (id INTEGER PRIMARY KEY, oops TEXT)`)}},
	}
	err := tampered.Migrate(context.Background(), db, migrations.DialectSQLite)
	if !errors.Is(err, migrations.ErrDrift) || !strings.Contains(err.Error(), "auth/1_users") {
		t.Fatalf("err = %v, want drift naming auth/1_users", err)
	}
}

func TestStatus_FreshDatabase(t *testing.T) {
	db := openDB(t)

	statuses, err := migrations.Status(context.Background(), db, migrations.DialectSQLite, fstest.MapFS{
		"0001_a.sql": sqlFile(`CREATE TABLE a (id INTEGER PRIMARY KEY)`),
	})
	if err != nil {
		t.Fatalf("fresh-DB migrations.Status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].State != migrations.StatePending {
		t.Fatalf("statuses = %+v, want one pending", statuses)
	}

	srcs := migrations.Sources{
		{Module: "b", FS: fstest.MapFS{"0001_x.sql": sqlFile(`SELECT 1`)}},
		{Module: "a", FS: fstest.MapFS{"0002_y.sql": sqlFile(`SELECT 1`)}},
	}
	statuses, err = srcs.Status(context.Background(), db, migrations.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Module != "a" || statuses[1].Module != "b" {
		t.Fatalf("statuses = %+v, want pending sorted by module", statuses)
	}
	for _, st := range statuses {
		if st.State != migrations.StatePending {
			t.Errorf("%s/%d: state = %s, want pending", st.Module, st.Version, st.State)
		}
	}
}

func TestSourcesStatus_MixedStatesOrdering(t *testing.T) {
	db := openDB(t)
	initial := migrations.Sources{
		{Module: "auth", FS: fstest.MapFS{
			"0001_users.sql":    sqlFile(`CREATE TABLE users (id INTEGER PRIMARY KEY)`),
			"0002_sessions.sql": sqlFile(`CREATE TABLE sessions (id INTEGER PRIMARY KEY)`),
		}},
		{Module: "todos", FS: fstest.MapFS{"0001_todos.sql": sqlFile(`CREATE TABLE todos (id INTEGER PRIMARY KEY)`)}},
	}
	if err := initial.Migrate(context.Background(), db, migrations.DialectSQLite); err != nil {
		t.Fatal(err)
	}

	inspect := migrations.Sources{
		{Module: "auth", FS: fstest.MapFS{
			"0001_users.sql": sqlFile(`CREATE TABLE users (id INTEGER PRIMARY KEY)`),
			"0003_roles.sql": sqlFile(`CREATE TABLE roles (id INTEGER PRIMARY KEY)`),
		}},
		{Module: "todos", FS: fstest.MapFS{"0001_todos.sql": sqlFile(`CREATE TABLE todos (id INTEGER PRIMARY KEY, oops TEXT)`)}},
	}
	statuses, err := inspect.Status(context.Background(), db, migrations.DialectSQLite)
	if err != nil {
		t.Fatal(err)
	}

	type row struct {
		module string
		v      int64
		state  migrations.State
	}
	var got []row
	for _, st := range statuses {
		got = append(got, row{st.Module, st.Version, st.State})
	}
	want := []row{
		{"auth", 1, migrations.StateApplied},
		{"auth", 2, migrations.StateOrphan},
		{"auth", 3, migrations.StatePending},
		{"todos", 1, migrations.StateDrifted},
	}
	if len(got) != len(want) {
		t.Fatalf("statuses = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statuses[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Empty module is a valid stream key.
func TestEmptyModuleIsOrdinary(t *testing.T) {
	db := openDB(t)
	fsys := fstest.MapFS{"0001_a.sql": sqlFile(`CREATE TABLE a (id INTEGER PRIMARY KEY)`)}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, fsys); err != nil {
		t.Fatal(err)
	}
	if err := (migrations.Sources{{Module: "", FS: fsys}}).Migrate(context.Background(), db, migrations.DialectSQLite); err != nil {
		t.Fatalf("hand-built empty module should be idempotent with single-stream call: %v", err)
	}

	dup := migrations.Sources{{FS: fsys}, {Module: "", FS: fsys}}
	if err := dup.Migrate(context.Background(), db, migrations.DialectSQLite); !errors.Is(err, migrations.ErrDuplicateModule) {
		t.Fatalf("duplicate empty module: err = %v, want migrations.ErrDuplicateModule", err)
	}
}

func TestMigrate_MultiStatementBody(t *testing.T) {
	db := openDB(t)
	src := fstest.MapFS{
		"0001_seed.sql": sqlFile(`
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO settings (key, value) VALUES ('theme', 'dark');
CREATE INDEX settings_value ON settings (value);
`),
	}
	if err := migrations.Migrate(context.Background(), db, migrations.DialectSQLite, src); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'theme'`).Scan(&value); err != nil || value != "dark" {
		t.Fatalf("seeded row: %q, %v", value, err)
	}
}
