package query

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
)

// DB binds an [Executor] to a [Dialect]. It also implements [Executor], so
// typed reads and generated writes can share one value:
//
//	q, err := query.New(db, query.DialectSQLite)
//	if err != nil { ... }
//	if _, err := q.Insert(ctx, "users", u); err != nil { ... }
//	users, err := query.All[User](ctx, q, "SELECT * FROM users")
//
// The free functions ([Insert], [All], [Tx], ...) remain available for callers
// that want to pass the executor and dialect explicitly.
type DB struct {
	exec    Executor
	dialect Dialect
}

// New binds a raw executor, such as *sql.DB, *sql.Tx, or a custom [Executor],
// to d. Passing an existing *DB is rejected; use it directly.
func New(exec Executor, d Dialect) (*DB, error) {
	if isNilExecutor(exec) {
		return nil, fmt.Errorf("query.New: nil executor")
	}
	if _, ok := exec.(*DB); ok {
		return nil, fmt.Errorf("query.New: executor is already a *query.DB; use it directly")
	}
	if err := checkDialect("query.New", d); err != nil {
		return nil, err
	}
	return &DB{exec: exec, dialect: d}, nil
}

// isNilExecutor catches typed nils stored in an interface.
func isNilExecutor(e Executor) bool {
	if e == nil {
		return true
	}
	switch v := reflect.ValueOf(e); v.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Chan, reflect.Slice, reflect.Interface:
		return v.IsNil()
	}
	return false
}

// Dialect returns the bound dialect.
func (q *DB) Dialect() Dialect { return q.dialect }

// QueryContext delegates to the bound executor.
func (q *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.exec.QueryContext(ctx, query, args...)
}

// ExecContext delegates to the bound executor.
func (q *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.exec.ExecContext(ctx, query, args...)
}

// Insert is [Insert] with the bound executor and dialect.
func (q *DB) Insert(ctx context.Context, table string, row any) (int64, error) {
	return Insert(ctx, q.exec, q.dialect, table, row)
}

// InsertMany is [InsertMany] with the bound executor and dialect.
func (q *DB) InsertMany(ctx context.Context, table string, rows any) error {
	return InsertMany(ctx, q.exec, q.dialect, table, rows)
}

// Update is [Update] with the bound executor and dialect.
func (q *DB) Update(ctx context.Context, table, where string, row any, whereArgs ...any) (int64, error) {
	return Update(ctx, q.exec, q.dialect, table, where, row, whereArgs...)
}

// Delete is [Delete] with the bound executor and dialect.
func (q *DB) Delete(ctx context.Context, table, where string, whereArgs ...any) (int64, error) {
	return Delete(ctx, q.exec, q.dialect, table, where, whereArgs...)
}

// Tx runs fn inside a transaction, passing a *DB bound to the *sql.Tx.
// The receiver must wrap a *sql.DB.
func (q *DB) Tx(ctx context.Context, fn func(*DB) error) error {
	db, ok := q.exec.(*sql.DB)
	if !ok {
		return fmt.Errorf("query: (*DB).Tx requires a handle over *sql.DB, got %T", q.exec)
	}
	return Tx(ctx, db, func(tx *sql.Tx) error {
		return fn(&DB{exec: tx, dialect: q.dialect})
	})
}
