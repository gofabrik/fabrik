// Package storetest is the conformance suite for [session.Store]
// implementations.
//
//	func TestMyStore(t *testing.T) {
//		storetest.Run(t, func() session.Store { return NewMyStore() })
//	}
//
// Run asserts CAS semantics, expiry filtering, Delete idempotency,
// byte-copy isolation, and optional capability behavior.
package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/session"
)

// Run executes the conformance suite against fresh stores.
func Run(t *testing.T, newStore func() session.Store) {
	t.Helper()
	t.Run("LoadMissing", func(t *testing.T) { testLoadMissing(t, newStore()) })
	t.Run("InsertAndRoundTrip", func(t *testing.T) { testInsertRoundTrip(t, newStore()) })
	t.Run("CAS", func(t *testing.T) { testCAS(t, newStore()) })
	t.Run("Expiry", func(t *testing.T) { testExpiry(t, newStore()) })
	t.Run("DeleteIdempotent", func(t *testing.T) { testDeleteIdempotent(t, newStore()) })
	t.Run("ByteCopyIsolation", func(t *testing.T) { testByteIsolation(t, newStore()) })
	t.Run("TTLBumper", func(t *testing.T) { testTTLBumper(t, newStore()) })
	t.Run("UserIndexer", func(t *testing.T) { testUserIndexer(t, newStore()) })
	t.Run("Scanner", func(t *testing.T) { testScanner(t, newStore()) })
	t.Run("Sweeper", func(t *testing.T) { testSweeper(t, newStore()) })
}

func live(sid, userID string, payload []byte) session.Record {
	return session.Record{
		SID:            sid,
		UserID:         userID,
		AbsoluteExpiry: time.Now().Add(time.Hour),
		IdleExpiry:     time.Now().Add(30 * time.Minute),
		Payload:        payload,
	}
}

func mustSave(t *testing.T, s session.Store, rec session.Record) session.Record {
	t.Helper()
	stored, err := s.Save(context.Background(), rec)
	if err != nil {
		t.Fatalf("save %s: %v", rec.SID, err)
	}
	return stored
}

func testLoadMissing(t *testing.T, s session.Store) {
	if _, err := s.Load(context.Background(), "nope"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("load missing: %v, want ErrNotFound", err)
	}
}

func testInsertRoundTrip(t *testing.T, s session.Store) {
	ctx := context.Background()
	stored := mustSave(t, s, live("a", "u1", []byte(`{"k":"v"}`)))
	if stored.Version != 1 {
		t.Fatalf("insert version = %d, want 1", stored.Version)
	}
	got, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != `{"k":"v"}` || got.UserID != "u1" || got.Version != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func testCAS(t *testing.T, s session.Store) {
	ctx := context.Background()
	stored := mustSave(t, s, live("a", "", []byte("{}")))

	// Version 0 inserts and conflicts if the SID exists.
	dup := live("a", "", []byte("{}"))
	if _, err := s.Save(ctx, dup); !errors.Is(err, session.ErrVersionConflict) {
		t.Fatalf("duplicate insert: %v, want ErrVersionConflict", err)
	}

	// Wrong nonzero versions conflict.
	wrong := stored
	wrong.Version = 99
	if _, err := s.Save(ctx, wrong); !errors.Is(err, session.ErrVersionConflict) {
		t.Fatalf("wrong version: %v, want ErrVersionConflict", err)
	}

	// The right version saves and increments.
	again := mustSave(t, s, stored)
	if again.Version != stored.Version+1 {
		t.Fatalf("version = %d, want %d", again.Version, stored.Version+1)
	}

	// Nonzero versions against missing records conflict.
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, again); !errors.Is(err, session.ErrVersionConflict) {
		t.Fatalf("save after delete: %v, want ErrVersionConflict", err)
	}
}

func testExpiry(t *testing.T, s session.Store) {
	ctx := context.Background()

	past := live("dead-absolute", "", []byte("{}"))
	past.AbsoluteExpiry = time.Now().Add(-time.Minute)
	mustSave(t, s, past)
	if _, err := s.Load(ctx, "dead-absolute"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("absolute-expired load: %v, want ErrNotFound", err)
	}

	idle := live("dead-idle", "", []byte("{}"))
	idle.IdleExpiry = time.Now().Add(-time.Minute)
	mustSave(t, s, idle)
	if _, err := s.Load(ctx, "dead-idle"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("idle-expired load: %v, want ErrNotFound", err)
	}

	// Zero IdleExpiry disables the idle deadline.
	none := live("no-idle", "", []byte("{}"))
	none.IdleExpiry = time.Time{}
	mustSave(t, s, none)
	if _, err := s.Load(ctx, "no-idle"); err != nil {
		t.Fatalf("zero IdleExpiry load: %v, want live record", err)
	}
}

func testDeleteIdempotent(t *testing.T, s session.Store) {
	ctx := context.Background()
	if err := s.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("delete missing: %v, want nil", err)
	}
	mustSave(t, s, live("a", "", []byte("{}")))
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("second delete: %v, want nil", err)
	}
}

func testByteIsolation(t *testing.T, s session.Store) {
	ctx := context.Background()
	payload := []byte(`{"k":"v"}`)
	rec := live("a", "", payload)
	stored := mustSave(t, s, rec)

	// Caller mutation after Save must not reach stored state.
	payload[2] = 'X'
	got, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != `{"k":"v"}` {
		t.Fatalf("store aliased caller bytes on Save: %q", got.Payload)
	}

	// Mutation of loaded bytes must not reach stored state.
	got.Payload[2] = 'Y'
	again, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Payload) != `{"k":"v"}` {
		t.Fatalf("store handed out internal bytes on Load: %q", again.Payload)
	}

	// The record returned by Save is isolated too.
	stored.Payload[2] = 'Z'
	final, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if string(final.Payload) != `{"k":"v"}` {
		t.Fatalf("Save returned aliased bytes: %q", final.Payload)
	}

	// The returned record must not alias the caller's input.
	in := []byte(`{"a":"b"}`)
	ret := mustSave(t, s, live("b", "", in))
	in[2] = 'X'
	if string(ret.Payload) != `{"a":"b"}` {
		t.Fatalf("returned record aliases caller input: %q", ret.Payload)
	}
}

func testTTLBumper(t *testing.T, s session.Store) {
	bumper, ok := s.(session.TTLBumper)
	if !ok {
		t.Skip("store does not implement TTLBumper")
	}
	ctx := context.Background()
	stored := mustSave(t, s, live("a", "", []byte("{}")))

	until := time.Now().Add(2 * time.Hour)
	if err := bumper.BumpTTL(ctx, "a", until); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IdleExpiry.Equal(until) {
		t.Fatalf("idle expiry = %v, want %v", got.IdleExpiry, until)
	}
	if got.Version != stored.Version {
		t.Fatalf("bump changed version: %d -> %d", stored.Version, got.Version)
	}
	if err := bumper.BumpTTL(ctx, "missing", until); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("bump missing: %v, want ErrNotFound", err)
	}
}

func testUserIndexer(t *testing.T, s session.Store) {
	idx, ok := s.(session.UserIndexer)
	if !ok {
		t.Skip("store does not implement UserIndexer")
	}
	ctx := context.Background()

	a := mustSave(t, s, live("a", "u1", []byte("{}")))
	mustSave(t, s, live("b", "u1", []byte("{}")))
	mustSave(t, s, live("c", "u2", []byte("{}")))

	sids, err := idx.ListByUser(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(sids)
	if !slices.Equal(sids, []string{"a", "b"}) {
		t.Fatalf("u1 sessions = %v", sids)
	}

	// A Save that changes UserID moves the index entry.
	moved := a
	moved.UserID = "u2"
	mustSave(t, s, moved)
	sids, _ = idx.ListByUser(ctx, "u1")
	if slices.Contains(sids, "a") {
		t.Fatalf("stale index: u1 still lists a: %v", sids)
	}
	sids, _ = idx.ListByUser(ctx, "u2")
	slices.Sort(sids)
	if !slices.Equal(sids, []string{"a", "c"}) {
		t.Fatalf("u2 sessions = %v", sids)
	}

	// A Delete drops the entry.
	if err := s.Delete(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	sids, _ = idx.ListByUser(ctx, "u2")
	if slices.Contains(sids, "c") {
		t.Fatalf("stale index after delete: %v", sids)
	}

	// Expired rows are filtered from listings.
	exp := live("d", "u3", []byte("{}"))
	exp.AbsoluteExpiry = time.Now().Add(-time.Minute)
	mustSave(t, s, exp)
	sids, _ = idx.ListByUser(ctx, "u3")
	if len(sids) != 0 {
		t.Fatalf("expired session listed: %v", sids)
	}

	// RevokeByUser honors except.
	mustSave(t, s, live("e", "u4", []byte("{}")))
	mustSave(t, s, live("f", "u4", []byte("{}")))
	n, err := idx.RevokeByUser(ctx, "u4", "e")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}
	if _, err := s.Load(ctx, "e"); err != nil {
		t.Fatalf("excepted session revoked: %v", err)
	}
	if _, err := s.Load(ctx, "f"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("f survived revocation: %v", err)
	}
}

func testSweeper(t *testing.T, s session.Store) {
	sweeper, ok := s.(session.Sweeper)
	if !ok {
		t.Skip("store does not implement Sweeper")
	}
	ctx := context.Background()
	mustSave(t, s, live("live", "", []byte("{}")))
	dead := live("dead", "", []byte("{}"))
	dead.AbsoluteExpiry = time.Now().Add(-time.Minute)
	mustSave(t, s, dead)

	n, err := sweeper.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept = %d, want 1", n)
	}
	if _, err := s.Load(ctx, "live"); err != nil {
		t.Fatalf("live session swept: %v", err)
	}
}

func testScanner(t *testing.T, s session.Store) {
	scanner, ok := s.(session.Scanner)
	if !ok {
		t.Skip("store does not implement Scanner")
	}
	ctx := context.Background()
	mustSave(t, s, live("a", "", []byte("{}")))
	mustSave(t, s, live("b", "", []byte("{}")))
	dead := live("dead", "", []byte("{}"))
	dead.AbsoluteExpiry = time.Now().Add(-time.Minute)
	mustSave(t, s, dead)

	var seen []string
	if err := scanner.Scan(ctx, func(sid string) bool {
		seen = append(seen, sid)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []string{"a", "b"}) {
		t.Fatalf("scan = %v, want live sessions only", seen)
	}

	// Iteration stops as soon as fn returns false.
	calls := 0
	if err := scanner.Scan(ctx, func(string) bool {
		calls++
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("scan after false = %d calls, want 1", calls)
	}
}
