// Package dbtest tests the PostgreSQL store implementation.
package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
	storepostgres "github.com/gofabrik/fabrik/auth/store/postgres"
	"github.com/gofabrik/fabrik/auth/store/storetest"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var pgSchemaCounter atomic.Int64

// openPostgres isolates each test through its connection search path.
func openPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, admin)
	schema := fmt.Sprintf("authtest_%d_%d", os.Getpid(), pgSchemaCounter.Add(1))
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil { // #nosec G202 -- generated test schema identifier, not user input
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", dsn+sep+"options="+url.QueryEscape("-csearch_path="+schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil { // #nosec G202 -- generated test schema identifier, not user input
			t.Errorf("drop test schema: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	})
	return db
}

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

func TestPostgresStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, now func() time.Time) store.Store {
		s, err := storepostgres.New(openPostgres(t), storepostgres.Options{AutoCreate: true, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestPostgresSchemaIdempotentAndManaged(t *testing.T) {
	db := openPostgres(t)
	if _, err := storepostgres.New(db, storepostgres.Options{AutoCreate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := storepostgres.New(db, storepostgres.Options{AutoCreate: true}); err != nil {
		t.Fatalf("second AutoCreate: %v", err)
	}

	managed := openPostgres(t)
	for _, stmt := range storepostgres.SchemaStatements() {
		if _, err := managed.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s, err := storepostgres.New(managed, storepostgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdentity(t.Context(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get against managed schema = %v, want ErrNotFound", err)
	}
}

// Foreign-key cascades must apply to deletes issued outside Store.
func TestPostgresForeignKeyCascade(t *testing.T) {
	ctx := t.Context()
	db := openPostgres(t)
	s, err := storepostgres.New(db, storepostgres.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePassword(ctx, "alice", "alice@example.com", "hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM identities WHERE id = $1`, []byte("alice")); err != nil {
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

func TestDeleteIdentityWithoutForeignKeys(t *testing.T) {
	ctx := t.Context()
	db := openPostgres(t)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s, err := storepostgres.New(db, storepostgres.Options{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	var replicationRole string
	if err := db.QueryRowContext(ctx, `SHOW session_replication_role`).Scan(&replicationRole); err != nil {
		t.Fatal(err)
	}
	if replicationRole != "replica" {
		t.Fatalf("session_replication_role = %q, want replica", replicationRole)
	}
	if _, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreatePassword(ctx, "alice", "alice@example.com", "old-hash"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteIdentity(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	var identities, credentials int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM identities),
		       (SELECT COUNT(*) FROM password_credentials)`).Scan(&identities, &credentials); err != nil {
		t.Fatal(err)
	}
	if identities != 0 || credentials != 0 {
		t.Fatalf("rows after DeleteIdentity = identities %d, credentials %d; want both zero", identities, credentials)
	}
	if _, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, "alice@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup after identity ID reuse = %v, want password.ErrNotFound", err)
	}
}
