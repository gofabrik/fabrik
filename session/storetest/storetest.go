// Package storetest is the conformance suite for [session.Store]
// implementations.
//
//	func TestMyStore(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) session.Store { return NewMyStore() })
//	}
//
// Run asserts CAS semantics, expiry filtering, Delete idempotency,
// byte-copy isolation, and optional capability behavior.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/session"
)

// Run executes the conformance suite with a fresh store per subtest.
func Run(t *testing.T, newStore func(t *testing.T) session.Store) {
	t.Helper()
	t.Run("LoadMissing", func(t *testing.T) { testLoadMissing(t, newStore(t)) })
	t.Run("InsertAndRoundTrip", func(t *testing.T) { testInsertRoundTrip(t, newStore(t)) })
	t.Run("CAS", func(t *testing.T) { testCAS(t, newStore(t)) })
	t.Run("SamePayloadReSave", func(t *testing.T) { testSamePayloadReSave(t, newStore(t)) })
	t.Run("Expiry", func(t *testing.T) { testExpiry(t, newStore(t)) })
	t.Run("DeleteIdempotent", func(t *testing.T) { testDeleteIdempotent(t, newStore(t)) })
	t.Run("ByteCopyIsolation", func(t *testing.T) { testByteIsolation(t, newStore(t)) })
	t.Run("LargePayload", func(t *testing.T) { testLargePayload(t, newStore(t)) })
	t.Run("CaseDistinctSIDs", func(t *testing.T) { testCaseDistinctSIDs(t, newStore(t)) })
	t.Run("TrailingSpaceSIDs", func(t *testing.T) { testTrailingSpaceSIDs(t, newStore(t)) })
	t.Run("BoundaryLengthSID", func(t *testing.T) { testBoundaryLengthSID(t, newStore(t)) })
	t.Run("TTLBumper", func(t *testing.T) { testTTLBumper(t, newStore(t)) })
	t.Run("TTLBumperMatchedButUnchanged", func(t *testing.T) { testTTLBumperMatchedButUnchanged(t, newStore(t)) })
	t.Run("TTLBumperConcurrentMatchedButUnchanged", func(t *testing.T) { testTTLBumperConcurrentMatchedButUnchanged(t, newStore(t)) })
	t.Run("BumpTTLInsertRace", func(t *testing.T) { testBumpTTLInsertRace(t, newStore(t)) })
	t.Run("UserIndexer", func(t *testing.T) { testUserIndexer(t, newStore(t)) })
	t.Run("UserIndexerByteExactIDs", func(t *testing.T) { testUserIndexerByteExactIDs(t, newStore(t)) })
	t.Run("BinarySafeIdentifiers", func(t *testing.T) { testBinarySafeIdentifiers(t, newStore(t)) })
	t.Run("Scanner", func(t *testing.T) { testScanner(t, newStore(t)) })
	t.Run("Sweeper", func(t *testing.T) { testSweeper(t, newStore(t)) })
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

func testSamePayloadReSave(t *testing.T, s session.Store) {
	// Saving an unchanged record still increments its version.
	ctx := context.Background()
	stored := mustSave(t, s, live("a", "u1", []byte(`{"k":"v"}`)))
	again, err := s.Save(ctx, stored)
	if err != nil {
		t.Fatalf("same-payload re-Save: %v", err)
	}
	if again.Version != stored.Version+1 {
		t.Fatalf("version = %d, want %d", again.Version, stored.Version+1)
	}
	got, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != again.Version || string(got.Payload) != `{"k":"v"}` {
		t.Fatalf("load after same-payload re-Save = %+v", got)
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

func testLargePayload(t *testing.T, s session.Store) {
	ctx := context.Background()
	large := bytes.Repeat([]byte("x"), 65537)
	mustSave(t, s, live("a", "", large))
	got, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Payload, large) {
		t.Fatalf("large payload did not round-trip (len got=%d want=%d)", len(got.Payload), len(large))
	}
}

func testCaseDistinctSIDs(t *testing.T, s session.Store) {
	ctx := context.Background()
	mustSave(t, s, live("Sid", "", []byte("upper")))
	mustSave(t, s, live("sid", "", []byte("lower")))
	got, err := s.Load(ctx, "Sid")
	if err != nil || string(got.Payload) != "upper" {
		t.Fatalf("Load 'Sid' = %q %v", got.Payload, err)
	}
	got, err = s.Load(ctx, "sid")
	if err != nil || string(got.Payload) != "lower" {
		t.Fatalf("Load 'sid' = %q %v", got.Payload, err)
	}
}

func testTrailingSpaceSIDs(t *testing.T, s session.Store) {
	ctx := context.Background()
	mustSave(t, s, live("a", "", []byte("no-space")))
	mustSave(t, s, live("a ", "", []byte("trailing-space")))
	got, err := s.Load(ctx, "a")
	if err != nil || string(got.Payload) != "no-space" {
		t.Fatalf("Load 'a' = %q %v", got.Payload, err)
	}
	got, err = s.Load(ctx, "a ")
	if err != nil || string(got.Payload) != "trailing-space" {
		t.Fatalf("Load 'a ' = %q %v", got.Payload, err)
	}
}

// testBoundaryLengthSID verifies the portable limit without compression.
func testBoundaryLengthSID(t *testing.T, s session.Store) {
	ctx := context.Background()
	sid := IncompressibleKey(2048)
	mustSave(t, s, live(sid, "", []byte("v")))
	got, err := s.Load(ctx, sid)
	if err != nil || string(got.Payload) != "v" {
		t.Fatalf("Load boundary sid = %q %v", got.Payload, err)
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

// testTTLBumperMatchedButUnchanged requires an unchanged live bump to succeed.
func testTTLBumperMatchedButUnchanged(t *testing.T, s session.Store) {
	bumper, ok := s.(session.TTLBumper)
	if !ok {
		t.Skip("store does not implement TTLBumper")
	}
	ctx := context.Background()
	until := time.Now().Add(2 * time.Hour)
	rec := live("a", "", []byte("{}"))
	rec.IdleExpiry = until
	stored := mustSave(t, s, rec)

	if err := bumper.BumpTTL(ctx, "a", until); err != nil {
		t.Fatalf("bump to the already-stored expiry: %v", err)
	}
	got, err := s.Load(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IdleExpiry.Equal(until) {
		t.Fatalf("idle expiry = %v, want %v", got.IdleExpiry, until)
	}
	if got.Version != stored.Version {
		t.Fatalf("matched-but-unchanged bump changed version: %d -> %d", stored.Version, got.Version)
	}
}

// testTTLBumperConcurrentMatchedButUnchanged requires every live bump to succeed.
func testTTLBumperConcurrentMatchedButUnchanged(t *testing.T, s session.Store) {
	bumper, ok := s.(session.TTLBumper)
	if !ok {
		t.Skip("store does not implement TTLBumper")
	}
	ctx := context.Background()
	until := time.Now().Add(2 * time.Hour)
	rec := live("a", "", []byte("{}"))
	rec.IdleExpiry = until
	mustSave(t, s, rec)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bumper.BumpTTL(ctx, "a", until); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent matched-but-unchanged bump: %v", err)
	}
}

// testBumpTTLInsertRace requires a successful bump to be visible after insertion.
func testBumpTTLInsertRace(t *testing.T, s session.Store) {
	bumper, ok := s.(session.TTLBumper)
	if !ok {
		t.Skip("store does not implement TTLBumper")
	}
	ctx := context.Background()
	bumpTo := time.Now().Add(3 * time.Hour).Truncate(time.Nanosecond)
	insertAt := time.Now().Add(1 * time.Hour).Truncate(time.Nanosecond)
	for i := range 50 {
		sid := "race-" + strconv.Itoa(i)
		start := make(chan struct{})
		var bumpErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			bumpErr = bumper.BumpTTL(ctx, sid, bumpTo)
		}()
		go func() {
			defer wg.Done()
			<-start
			rec := live(sid, "", []byte("{}"))
			rec.IdleExpiry = insertAt
			if _, err := s.Save(ctx, rec); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		if bumpErr != nil && !errors.Is(bumpErr, session.ErrNotFound) {
			t.Fatalf("bump: %v", bumpErr)
		}
		rec, err := s.Load(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if bumpErr == nil && !rec.IdleExpiry.Equal(bumpTo) {
			t.Fatalf("iteration %d: bump reported success but idle expiry is %v, want %v", i, rec.IdleExpiry, bumpTo)
		}
	}
}

// testBinarySafeIdentifiers verifies NUL and invalid UTF-8 identifiers.
func testBinarySafeIdentifiers(t *testing.T, s session.Store) {
	ctx := context.Background()
	sid := "sid\xff\xfe"
	rec := live(sid, "u\x00id", []byte("{}"))
	mustSave(t, s, rec)
	got, err := s.Load(ctx, sid)
	if err != nil || got.UserID != "u\x00id" {
		t.Fatalf("binary sid load: %+v err=%v", got, err)
	}
	if idx, ok := s.(session.UserIndexer); ok {
		sids, err := idx.ListByUser(ctx, "u\x00id")
		if err != nil || len(sids) != 1 || sids[0] != sid {
			t.Fatalf("ListByUser binary id = %v %v", sids, err)
		}
	}
}

// testUserIndexerByteExactIDs requires byte-exact user ID indexing.
func testUserIndexerByteExactIDs(t *testing.T, s session.Store) {
	idx, ok := s.(session.UserIndexer)
	if !ok {
		t.Skip("store does not implement UserIndexer")
	}
	ctx := context.Background()
	mustSave(t, s, live("s1", "User", []byte("{}")))
	mustSave(t, s, live("s2", "user", []byte("{}")))
	mustSave(t, s, live("s3", "user ", []byte("{}")))
	for id, want := range map[string]string{"User": "s1", "user": "s2", "user ": "s3"} {
		sids, err := idx.ListByUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(sids) != 1 || sids[0] != want {
			t.Fatalf("ListByUser(%q) = %v, want [%s]", id, sids, want)
		}
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

// IncompressibleKey returns deterministic data that resists compression.
func IncompressibleKey(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var b strings.Builder
	b.Grow(n)
	state := uint64(0x9E3779B97F4A7C15)
	for range n {
		state = state*6364136223846793005 + 1442695040888963407
		b.WriteByte(alphabet[state>>58])
	}
	return b.String()
}
