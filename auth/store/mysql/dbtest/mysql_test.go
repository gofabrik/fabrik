// Package dbtest tests the MySQL and MariaDB store implementations.
package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
	storemysql "github.com/gofabrik/fabrik/auth/store/mysql"
	"github.com/gofabrik/fabrik/auth/store/storetest"
)

func waitReady(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("database not ready: %v", err)
}

var mysqlDBCounter atomic.Int64

// openMySQL isolates each test in its own database.
func openMySQL(t *testing.T, envVar string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(envVar)
	if dsn == "" {
		t.Skipf("%s not set", envVar)
	}
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, admin)
	name := fmt.Sprintf("authtest_%d_%d", os.Getpid(), mysqlDBCounter.Add(1))
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil { // #nosec G202 -- generated test database identifier, not user input
		t.Fatal(err)
	}
	cfg.DBName = name
	// The store must not depend on multi-statement execution.
	cfg.MultiStatements = false
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil { // #nosec G202 -- generated test database identifier, not user input
			t.Errorf("drop test database: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	})
	return db
}

func newFactory(envVar string) storetest.Factory {
	return func(t *testing.T, now func() time.Time) store.Store {
		s, err := storemysql.New(openMySQL(t, envVar), storemysql.Options{AutoCreate: true, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

func TestMySQLStoreConformance(t *testing.T) {
	storetest.Run(t, newFactory("TEST_MYSQL_DSN"))
}

func TestMariaDBStoreConformance(t *testing.T) {
	storetest.Run(t, newFactory("TEST_MARIADB_DSN"))
}

func testSchemaIdempotentAndManaged(t *testing.T, envVar string) {
	db := openMySQL(t, envVar)
	if _, err := storemysql.New(db, storemysql.Options{AutoCreate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := storemysql.New(db, storemysql.Options{AutoCreate: true}); err != nil {
		t.Fatalf("second AutoCreate: %v", err)
	}

	managed := openMySQL(t, envVar)
	for _, stmt := range storemysql.SchemaStatements() {
		if _, err := managed.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s, err := storemysql.New(managed, storemysql.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdentity(t.Context(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get against managed schema = %v, want ErrNotFound", err)
	}
}

func TestMySQLSchemaIdempotentAndManaged(t *testing.T) {
	testSchemaIdempotentAndManaged(t, "TEST_MYSQL_DSN")
}

func TestMariaDBSchemaIdempotentAndManaged(t *testing.T) {
	testSchemaIdempotentAndManaged(t, "TEST_MARIADB_DSN")
}

// Foreign-key cascades must apply to deletes issued outside Store.
func testForeignKeyCascade(t *testing.T, envVar string) {
	ctx := t.Context()
	db := openMySQL(t, envVar)
	s, err := storemysql.New(db, storemysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePassword(ctx, "alice", "alice@example.com", "hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, []byte("alice")); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM password_credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("credential rows after cascade = %d, want 0", n)
	}
	if _, err := s.Lookup(ctx, "alice@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup after cascade = %v, want password.ErrNotFound", err)
	}
}

func TestMySQLForeignKeyCascade(t *testing.T) {
	testForeignKeyCascade(t, "TEST_MYSQL_DSN")
}

func TestMariaDBForeignKeyCascade(t *testing.T) {
	testForeignKeyCascade(t, "TEST_MARIADB_DSN")
}

// Concurrent claims for one email return ErrEmailTaken to the loser.
func testConcurrentEmailClaim(t *testing.T, envVar string) {
	ctx := t.Context()
	db := openMySQL(t, envVar)
	s, err := storemysql.New(db, storemysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	for round := range 20 {
		a := fmt.Sprintf("a%d", round)
		b := fmt.Sprintf("b%d", round)
		email := fmt.Sprintf("shared%d@example.com", round)
		for _, id := range []string{a, b} {
			if _, err := s.CreateIdentity(ctx, id, store.IdentityClaims{}); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, id := range []string{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.CreatePassword(ctx, id, email, "hash-"+id)
			}()
		}
		close(start)
		wg.Wait()
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case errors.Is(err, store.ErrEmailTaken):
			default:
				t.Fatalf("round %d: CreatePassword[%d] = %v, want nil or ErrEmailTaken", round, i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: winners = %d, want 1", round, winners)
		}
	}
}

func TestMySQLConcurrentEmailClaim(t *testing.T) {
	testConcurrentEmailClaim(t, "TEST_MYSQL_DSN")
}

func TestMariaDBConcurrentEmailClaim(t *testing.T) {
	testConcurrentEmailClaim(t, "TEST_MARIADB_DSN")
}

// Concurrent identical CAS writes allow one success unless the write is a no-op.
func testSameTargetCAS(t *testing.T, envVar string) {
	ctx := t.Context()
	db := openMySQL(t, envVar)
	s, err := storemysql.New(db, storemysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{}); err != nil {
		t.Fatal(err)
	}
	for round := range 20 {
		old := fmt.Sprintf("old-%d", round)
		next := fmt.Sprintf("new-%d", round)
		if err := s.SetPassword(ctx, "alice", "alice@example.com", old); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.UpdateHash(ctx, "alice@example.com", old, next)
			}()
		}
		close(start)
		wg.Wait()
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case errors.Is(err, password.ErrHashChanged):
			default:
				t.Fatalf("round %d: UpdateHash[%d] = %v, want nil or ErrHashChanged", round, i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: winners = %d, want 1", round, winners)
		}
		cred, err := s.Lookup(ctx, "alice@example.com")
		if err != nil || cred.Hash != next {
			t.Fatalf("round %d: Lookup = %q, %v", round, cred.Hash, err)
		}
	}

	if err := s.UpdateHash(ctx, "alice@example.com", "new-19", "new-19"); err != nil {
		t.Fatalf("no-op UpdateHash: %v", err)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != "new-19" {
		t.Fatalf("Lookup after no-op = %q, %v", cred.Hash, err)
	}
	if err := s.UpdateHash(ctx, "alice@example.com", "stale", "stale"); !errors.Is(err, password.ErrHashChanged) {
		t.Fatalf("UpdateHash(stale no-op) = %v, want ErrHashChanged", err)
	}
}

func TestMySQLSameTargetCAS(t *testing.T) {
	testSameTargetCAS(t, "TEST_MYSQL_DSN")
}

func TestMariaDBSameTargetCAS(t *testing.T) {
	testSameTargetCAS(t, "TEST_MARIADB_DSN")
}

// Concurrent creates for one identity return ErrExists to the loser.
func testConcurrentOwnCreate(t *testing.T, envVar string) {
	ctx := t.Context()
	db := openMySQL(t, envVar)
	s, err := storemysql.New(db, storemysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	for round := range 20 {
		id := fmt.Sprintf("own%d", round)
		email := fmt.Sprintf("own%d@example.com", round)
		if _, err := s.CreateIdentity(ctx, id, store.IdentityClaims{}); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.CreatePassword(ctx, id, email, "hash-"+strconv.Itoa(i))
			}()
		}
		close(start)
		wg.Wait()
		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case errors.Is(err, store.ErrExists):
			default:
				t.Fatalf("round %d: CreatePassword[%d] = %v, want nil or ErrExists", round, i, err)
			}
		}
		if winners != 1 {
			t.Fatalf("round %d: winners = %d, want 1", round, winners)
		}
	}
}

func TestMySQLConcurrentOwnCreate(t *testing.T) {
	testConcurrentOwnCreate(t, "TEST_MYSQL_DSN")
}

func TestMariaDBConcurrentOwnCreate(t *testing.T) {
	testConcurrentOwnCreate(t, "TEST_MARIADB_DSN")
}

// Concurrent SetPassword calls both succeed and persist one supplied hash.
func testConcurrentSetPassword(t *testing.T, envVar string) {
	ctx := t.Context()
	db := openMySQL(t, envVar)
	s, err := storemysql.New(db, storemysql.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{}); err != nil {
		t.Fatal(err)
	}
	for round := range 20 {
		email := "alice@example.com"
		hashes := []string{
			fmt.Sprintf("hash-a-%d", round),
			fmt.Sprintf("hash-b-%d", round),
		}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = s.SetPassword(ctx, "alice", email, hashes[i])
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: SetPassword[%d] = %v", round, i, err)
			}
		}
		cred, err := s.Lookup(ctx, email)
		if err != nil || (cred.Hash != hashes[0] && cred.Hash != hashes[1]) {
			t.Fatalf("round %d: Lookup = %q, %v", round, cred.Hash, err)
		}
	}
}

func TestMySQLConcurrentSetPassword(t *testing.T) {
	testConcurrentSetPassword(t, "TEST_MYSQL_DSN")
}

func TestMariaDBConcurrentSetPassword(t *testing.T) {
	testConcurrentSetPassword(t, "TEST_MARIADB_DSN")
}
