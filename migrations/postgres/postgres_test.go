package postgres

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeState controls advisory-lock responses and tracks connection reuse.
type fakeState struct {
	unlockErr      error
	unlockReleased bool
	opens          atomic.Int32
}

type fakeDriver struct{ st *fakeState }

func (d fakeDriver) Open(string) (sqldriver.Conn, error) {
	d.st.opens.Add(1)
	return &fakeConn{st: d.st}, nil
}

type fakeConn struct{ st *fakeState }

func (c *fakeConn) Prepare(string) (sqldriver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *fakeConn) Begin() (sqldriver.Tx, error) { return nil, errors.New("begin unsupported") }
func (c *fakeConn) Close() error                 { return nil }

func (c *fakeConn) QueryContext(_ context.Context, q string, _ []sqldriver.NamedValue) (sqldriver.Rows, error) {
	if strings.Contains(q, "pg_advisory_unlock") {
		if c.st.unlockErr != nil {
			return nil, c.st.unlockErr
		}
		return &oneValueRows{val: c.st.unlockReleased}, nil
	}
	return nil, fmt.Errorf("unexpected query %q", q)
}

func (c *fakeConn) ExecContext(_ context.Context, q string, _ []sqldriver.NamedValue) (sqldriver.Result, error) {
	switch {
	case strings.Contains(q, "pg_advisory_lock"):
		return sqldriver.ResultNoRows, nil
	case strings.Contains(q, "pg_advisory_unlock"):
		if c.st.unlockErr != nil {
			return nil, c.st.unlockErr
		}
		return sqldriver.ResultNoRows, nil
	}
	return nil, fmt.Errorf("unexpected exec %q", q)
}

type oneValueRows struct {
	val  any
	done bool
}

func (r *oneValueRows) Columns() []string { return []string{"v"} }
func (r *oneValueRows) Close() error      { return nil }
func (r *oneValueRows) Next(dest []sqldriver.Value) error {
	if r.done {
		return errors.New("no more rows")
	}
	r.done = true
	dest[0] = r.val
	return nil
}

var driverSeq atomic.Int64

func openFake(t *testing.T, st *fakeState) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("fake-pg-%d", driverSeq.Add(1))
	sql.Register(name, fakeDriver{st: st})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	return db
}

func openSession(t *testing.T, db *sql.DB) interface{ Close() error } {
	t.Helper()
	sess, err := Driver().OpenSession(context.Background(), db)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return sess
}

func TestCloseUnlockErrorEvictsConnection(t *testing.T) {
	st := &fakeState{unlockErr: errors.New("server gone")}
	db := openFake(t, st)
	sess := openSession(t, db)

	err := sess.Close()
	if err == nil || !strings.Contains(err.Error(), "release pg advisory lock") {
		t.Fatalf("Close error = %v, want release failure", err)
	}
	if n := freshOpens(t, db, st); n != 2 {
		t.Fatalf("driver opens after reacquire = %d, want 2 (eviction); the session conn may still hold the lock", n)
	}
}

func TestCloseNotHeldEvictsConnection(t *testing.T) {
	st := &fakeState{unlockReleased: false}
	db := openFake(t, st)
	sess := openSession(t, db)

	err := sess.Close()
	if err == nil || !strings.Contains(err.Error(), "not held") {
		t.Fatalf("Close error = %v, want not-held failure", err)
	}
	if n := freshOpens(t, db, st); n != 2 {
		t.Fatalf("driver opens after reacquire = %d, want 2 (eviction after pg_advisory_unlock returned false)", n)
	}
}

func TestCloseReleasedReturnsConnectionToPool(t *testing.T) {
	st := &fakeState{unlockReleased: true}
	db := openFake(t, st)
	sess := openSession(t, db)

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := freshOpens(t, db, st); n != 1 {
		t.Fatalf("driver opens after reacquire = %d, want 1 (pooled after successful unlock)", n)
	}
}

func freshOpens(t *testing.T, db *sql.DB, st *fakeState) int32 {
	t.Helper()
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	defer c.Close() //nolint:errcheck
	return st.opens.Load()
}
