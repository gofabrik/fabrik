package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// serve runs one request through the middleware. The handler sees the
// session context; the returned recorder carries any Set-Cookie.
func serve(t *testing.T, m *Manager[appSession], sid string, handler func(w http.ResponseWriter, r *http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	}
	rr := httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(handler)).ServeHTTP(rr, req)
	return rr
}

// sessionCookie returns the response's session cookie value and
// whether any session Set-Cookie was emitted. A cleared cookie reads
// as ("", true).
func sessionCookie(t *testing.T, rr *httptest.ResponseRecorder) (string, bool) {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == "sid" {
			return c.Value, true
		}
	}
	return "", false
}

// plainStore forwards only the three required methods, hiding every
// optional capability.
type plainStore struct{ inner Store }

func (p plainStore) Load(ctx context.Context, sid string) (Record, error) {
	return p.inner.Load(ctx, sid)
}
func (p plainStore) Save(ctx context.Context, rec Record) (Record, error) {
	return p.inner.Save(ctx, rec)
}
func (p plainStore) Delete(ctx context.Context, sid string) error {
	return p.inner.Delete(ctx, sid)
}

// hookStore wraps a Store with injection points and an operation log.
type hookStore struct {
	inner Store

	mu           sync.Mutex
	ops          []string
	beforeSave   func(rec Record) error
	beforeDelete func(sid string) error
}

func (h *hookStore) log(op string) {
	h.mu.Lock()
	h.ops = append(h.ops, op)
	h.mu.Unlock()
}

func (h *hookStore) opLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ops...)
}

func (h *hookStore) Load(ctx context.Context, sid string) (Record, error) {
	h.log("load")
	return h.inner.Load(ctx, sid)
}

func (h *hookStore) Save(ctx context.Context, rec Record) (Record, error) {
	if h.beforeSave != nil {
		if err := h.beforeSave(rec); err != nil {
			return Record{}, err
		}
	}
	h.log("save:" + rec.SID)
	return h.inner.Save(ctx, rec)
}

func (h *hookStore) Delete(ctx context.Context, sid string) error {
	if h.beforeDelete != nil {
		if err := h.beforeDelete(sid); err != nil {
			return err
		}
	}
	h.log("delete:" + sid)
	return h.inner.Delete(ctx, sid)
}

func TestFreshSessionZeroValueAndStagedView(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		v, err := h.Get(ctx)
		if err != nil || v.Name != "" {
			t.Errorf("fresh Get = %+v, %v", v, err)
		}
		if ok, _ := h.Has(ctx); ok {
			t.Error("fresh Has = true")
		}
		if err := h.Save(ctx, appSession{Name: "alice"}); err != nil {
			t.Fatal(err)
		}
		// Save is visible before commit.
		if ok, _ := h.Has(ctx); !ok {
			t.Error("Has false after staged Save")
		}
		if v, _ := h.Get(ctx); v.Name != "alice" {
			t.Errorf("staged Get = %+v", v)
		}
		if err := h.Clear(ctx); err != nil {
			t.Fatal(err)
		}
		if ok, _ := h.Has(ctx); ok {
			t.Error("Has true after staged Clear")
		}
		_ = h.Save(ctx, appSession{Name: "bob"})
	})

	sid, ok := sessionCookie(t, rr)
	if !ok || sid == "" {
		t.Fatal("staged save did not mint a session cookie")
	}
	got, err := h.Load(context.Background(), sid)
	if err != nil || got.Name != "bob" {
		t.Fatalf("stored value = %+v, %v", got, err)
	}
}

func TestReadsNeverMint(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		_, _ = h.Has(r.Context())
		_, _ = m.SID(r.Context())
	})
	if _, ok := sessionCookie(t, rr); ok {
		t.Error("read-only fresh visit emitted a token")
	}
	for _, op := range store.opLog() {
		if strings.HasPrefix(op, "save") {
			t.Errorf("read-only fresh visit wrote to the store: %v", store.opLog())
		}
	}
}

func TestUntouchedRequestWithDeadCookieClearsNothing(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store })

	rr := serve(t, m, "dead-sid", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if _, ok := sessionCookie(t, rr); ok {
		t.Error("untouched request cleared the dead cookie - discovery must stay lazy")
	}
	if len(store.opLog()) != 0 {
		t.Errorf("untouched request touched the store: %v", store.opLog())
	}
}

func TestTouchedStaleTokenClearsAtCommit(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "dead-sid", func(w http.ResponseWriter, r *http.Request) {
		v, err := h.Get(r.Context())
		if err != nil || v.Name != "" {
			t.Errorf("stale-token Get = %+v, %v", v, err)
		}
		if sid, _ := m.SID(r.Context()); sid != "" {
			t.Errorf("SID on stale token = %q, want empty", sid)
		}
	})
	val, ok := sessionCookie(t, rr)
	if !ok || val != "" {
		t.Fatalf("stale token not cleared: (%q, %v)", val, ok)
	}
}

func TestStaleTokenSetSupersedesClear(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "dead-sid", func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		_ = h.Save(r.Context(), appSession{Name: "alice"})
	})
	cookies := rr.Result().Cookies()
	var sessionCookies []*http.Cookie
	for _, c := range cookies {
		if c.Name == "sid" {
			sessionCookies = append(sessionCookies, c)
		}
	}
	if len(sessionCookies) != 1 {
		t.Fatalf("commit emitted %d token instructions, want exactly 1", len(sessionCookies))
	}
	if sessionCookies[0].Value == "" {
		t.Fatal("clear won over set")
	}
}

func TestSingleCommitForMultipleDirtyHandles(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	ha := m.app
	hb, _ := Use(m, NewKey[otherShape]("other"))

	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = ha.Save(r.Context(), appSession{Name: "a"})
		_ = hb.Save(r.Context(), otherShape{Count: 2})
	})
	saves := 0
	for _, op := range store.opLog() {
		if strings.HasPrefix(op, "save") {
			saves++
		}
	}
	if saves != 1 {
		t.Fatalf("two dirty handles produced %d store writes, want 1: %v", saves, store.opLog())
	}
}

func TestSaveThenUpdateFoldsAndConsumes(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app

	var sid string
	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_ = h.Save(ctx, appSession{Name: "staged"})
		if err := h.Update(ctx, func(s *appSession) error {
			// Update's working value starts from the staged value.
			if s.Name != "staged" {
				t.Errorf("Update base = %q, want staged value", s.Name)
			}
			s.Name = "updated"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sid, _ = m.SID(r.Context())
	})

	// Update consumes the stage and avoids a second commit write.
	saves := 0
	for _, op := range store.opLog() {
		if strings.HasPrefix(op, "save") {
			saves++
		}
	}
	if saves != 1 {
		t.Fatalf("Save-then-Update produced %d writes, want 1: %v", saves, store.opLog())
	}
	_ = sid
}

func TestUpdateClosureErrorAbortsCleanly(t *testing.T) {
	m := newTestManager(t)
	h := m.app
	boom := errors.New("boom")

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_ = h.Save(ctx, appSession{Name: "staged"})
		if err := h.Update(ctx, func(s *appSession) error { return boom }); !errors.Is(err, boom) {
			t.Errorf("closure error = %v", err)
		}
		// The stage survives the aborted Update.
		if v, _ := h.Get(ctx); v.Name != "staged" {
			t.Errorf("staged state after aborted Update = %+v", v)
		}
	})
	sid, _ := sessionCookie(t, rr)
	got, err := h.Load(context.Background(), sid)
	if err != nil || got.Name != "staged" {
		t.Fatalf("committed value = %+v, %v", got, err)
	}
}

func TestUpdateMintsImmediatelyPreCommit(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Update(r.Context(), func(s *appSession) error {
			s.Name = "minted"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// Update writes before response start.
		found := false
		for _, op := range store.opLog() {
			if strings.HasPrefix(op, "save") {
				found = true
			}
		}
		if !found {
			t.Error("pre-commit Update did not write immediately")
		}
	})
	sid, ok := sessionCookie(t, rr)
	if !ok || sid == "" {
		t.Fatal("minting Update left its token behind")
	}
	got, err := h.Load(context.Background(), sid)
	if err != nil || got.Name != "minted" {
		t.Fatalf("minted value = %+v, %v", got, err)
	}
}

func TestCommitReMergePreservesOtherCells(t *testing.T) {
	m := newTestManager(t)
	ha := m.app
	hb, _ := Use(m, NewKey[otherShape]("other"))

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = ha.Save(r.Context(), appSession{Name: "v1"})
	})
	sid, _ := sessionCookie(t, rr)

	// An out-of-band write bumps the version mid-request.
	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if _, err := ha.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		_ = hb.Save(r.Context(), otherShape{Count: 42})
		if err := ha.UpdateSID(context.Background(), sid, func(s *appSession) error {
			s.Name = "out-of-band"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	// Re-merge preserves the out-of-band cell and lands the staged one.
	a, err := ha.Load(context.Background(), sid)
	if err != nil || a.Name != "out-of-band" {
		t.Fatalf("cell a = %+v, %v (re-merge clobbered an untouched cell)", a, err)
	}
	b, err := hb.Load(context.Background(), sid)
	if err != nil || b.Count != 42 {
		t.Fatalf("cell b = %+v, %v", b, err)
	}
}

func TestCommitSameCellStagedSaveWins(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "v1"})
	})
	sid, _ := sessionCookie(t, rr)

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		_ = h.Save(r.Context(), appSession{Name: "staged-wins"})
		_ = h.UpdateSID(context.Background(), sid, func(s *appSession) error {
			s.Name = "out-of-band"
			return nil
		})
	})

	got, _ := h.Load(context.Background(), sid)
	if got.Name != "staged-wins" {
		t.Fatalf("same-cell conflict resolved to %q, want the staged save", got.Name)
	}
}

func TestCommitDeletedRecordStaysGone(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "v1"})
	})
	sid, _ := sessionCookie(t, rr)

	rr2 := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		_ = h.Save(r.Context(), appSession{Name: "resurrect?"})
		// Revocation wins over a pending save.
		_ = m.DestroySID(context.Background(), sid)
	})
	if rr2.Code != http.StatusInternalServerError {
		t.Fatalf("commit against a revoked session = %d, want 500", rr2.Code)
	}
	if _, err := h.Load(context.Background(), sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked session resurrected: %v", err)
	}
}

func TestCommitConflictExhaustionSurfaces(t *testing.T) {
	mem := NewMemoryStore()
	store := &hookStore{inner: mem}
	m := newTestManager(t, func(c *Config) { c.Store = store; c.MaxRetries = 1 })
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "v1"})
	})
	sid, _ := sessionCookie(t, rr)

	// Every commit save loses the race.
	var inHook bool
	store.beforeSave = func(rec Record) error {
		if inHook || rec.SID != sid || rec.Version == 0 {
			return nil
		}
		inHook = true
		defer func() { inHook = false }()
		cur, err := mem.Load(context.Background(), sid)
		if err != nil {
			return nil
		}
		_, _ = mem.Save(context.Background(), cur)
		return nil
	}
	rr2 := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		_ = h.Save(r.Context(), appSession{Name: "loser"})
	})
	if rr2.Code != http.StatusInternalServerError {
		t.Fatalf("exhausted retries = %d, want 500", rr2.Code)
	}
	if !strings.Contains(rr2.Body.String(), "session commit failed") {
		t.Fatalf("commit failure body: %q", rr2.Body.String())
	}
}

func TestHandlerWritesNothingStillCommits(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "silent"})
		// Return without writing anything.
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("implicit status = %d, want 200", rr.Code)
	}
	if _, ok := sessionCookie(t, rr); !ok {
		t.Fatal("implicit 200 lost the Set-Cookie")
	}
}

func TestRedirectCommitsBeforeHeaders(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "redirected"})
		http.Redirect(w, r, "/next", http.StatusSeeOther)
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rr.Code)
	}
	if _, ok := sessionCookie(t, rr); !ok {
		t.Fatal("redirect lost the Set-Cookie - commit must run at WriteHeader")
	}
}

func TestSIDCollisionRemintsBounded(t *testing.T) {
	sids := []string{"dup", "dup", "unique"}
	i := 0
	m := newTestManager(t, func(c *Config) {
		c.NewSID = func() (string, error) { s := sids[i%len(sids)]; i++; return s, nil }
	})
	h := m.app

	// Occupy "dup".
	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "first"})
	})
	if sid, _ := sessionCookie(t, rr); sid != "dup" {
		t.Fatalf("first mint = %q", sid)
	}

	// The second mint collides twice, then re-mints to "unique".
	rr2 := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "second"})
	})
	if sid, _ := sessionCookie(t, rr2); sid != "unique" {
		t.Fatalf("re-mint = %q, want unique", sid)
	}

	// A generator that only collides exhausts instead of looping.
	always := newTestManager(t, func(c *Config) {
		c.NewSID = func() (string, error) { return "dup2", nil }
		c.MaxRetries = 1
	})
	ha := always.app
	serve(t, always, "", func(w http.ResponseWriter, r *http.Request) {
		_ = ha.Save(r.Context(), appSession{Name: "x"})
	})
	rr3 := serve(t, always, "", func(w http.ResponseWriter, r *http.Request) {
		_ = ha.Save(r.Context(), appSession{Name: "y"})
	})
	if rr3.Code != http.StatusInternalServerError {
		t.Fatalf("collision exhaustion = %d, want 500", rr3.Code)
	}
}

func TestEmptyNewSIDIsGeneratorFailure(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) {
		c.Store = store
		c.NewSID = func() (string, error) { return "", nil }
	})
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "x"})
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("empty SID mint = %d, want 500", rr.Code)
	}
	for _, op := range store.opLog() {
		if strings.HasPrefix(op, "save:") && strings.TrimPrefix(op, "save:") == "" {
			t.Fatal("empty SID reached the Store")
		}
	}
}

func TestCanonicalEmptyPayload(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "v"})
	})
	sid, _ := sessionCookie(t, rr)

	// Clearing the last cell keeps the session record alive.
	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if err := h.Clear(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	rec, err := mem.Load(context.Background(), sid)
	if err != nil {
		t.Fatalf("clearing the last cell ended the session: %v", err)
	}
	if string(rec.Payload) != "{}" {
		t.Fatalf("empty payload = %q, want {}", rec.Payload)
	}
}

func TestClearAbsentStagesNothing(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Clear(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if _, ok := sessionCookie(t, rr); ok {
		t.Error("Clear on absent minted a session")
	}
	for _, op := range store.opLog() {
		if strings.HasPrefix(op, "save") {
			t.Errorf("Clear on absent wrote: %v", store.opLog())
		}
	}
}

func TestNoMiddlewareAndWrongManager(t *testing.T) {
	m1 := newTestManager(t)
	m2 := newTestManager(t)
	h1 := m1.app

	// Bare contexts return ErrNoSession.
	if _, err := h1.Get(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("bare context: %v, want ErrNoSession", err)
	}
	if _, err := m1.SID(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("bare context SID: %v", err)
	}

	// Request state is keyed per manager.
	serve(t, m2, "", func(w http.ResponseWriter, r *http.Request) {
		if _, err := h1.Get(r.Context()); !errors.Is(err, ErrNoSession) {
			t.Errorf("wrong manager: %v, want ErrNoSession", err)
		}
	})
}

func TestUpdateClosureRunsOutsideLock(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		inClosure := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = h.Update(ctx, func(s *appSession) error {
				close(inClosure)
				<-release
				s.Name = "updated"
				return nil
			})
		}()
		<-inClosure
		// A concurrent read must not deadlock while the closure runs.
		got := make(chan struct{})
		go func() {
			_, _ = h.Get(ctx)
			close(got)
		}()
		select {
		case <-got:
		case <-time.After(2 * time.Second):
			t.Error("Get deadlocked while an Update closure was running")
		}
		close(release)
		<-done
	})
}

func TestUpdateSnapshotRaceDirtyFlagRule(t *testing.T) {
	m := newTestManager(t)
	h := m.app

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "v1"})
	})
	sid, _ := sessionCookie(t, rr)

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_ = h.Save(ctx, appSession{Name: "staged"})

		inClosure := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = h.Update(ctx, func(s *appSession) error {
				close(inClosure)
				<-release
				s.Name = "from-update"
				return nil
			})
		}()
		<-inClosure
		// Save during the closure re-dirties the cell and wins.
		_ = h.Save(ctx, appSession{Name: "late-save-wins"})
		close(release)
		<-done
	})

	got, _ := h.Load(context.Background(), sid)
	if got.Name != "late-save-wins" {
		t.Fatalf("final value = %q, want the late save (deterministic last-writer-wins)", got.Name)
	}
}
