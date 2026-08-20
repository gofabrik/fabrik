package dbtest

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

// stubDriver permits transaction setup without a server.
type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("stub: no statements") }
func (stubConn) Close() error                        { return nil }
func (stubConn) Begin() (driver.Tx, error)           { return stubTx{}, nil }

type stubTx struct{}

func (stubTx) Commit() error   { return nil }
func (stubTx) Rollback() error { return nil }

func init() {
	sql.Register("jobspins-stub", stubDriver{})
}

func finishedTx(t *testing.T) *sql.Tx {
	t.Helper()
	db, err := sql.Open("jobspins-stub", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // best-effort cleanup
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	return tx
}
