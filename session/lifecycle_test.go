package session

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// establish mints a session with one saved cell and returns its SID.
func establish(t *testing.T, m *Manager[appSession], h *Handle[appSession], name string) string {
	t.Helper()
	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Save(r.Context(), appSession{Name: name}); err != nil {
			t.Fatal(err)
		}
	})
	sid, ok := sessionCookie(t, rr)
	if !ok || sid == "" {
		t.Fatal("establish: no session cookie")
	}
	return sid
}

func TestRenewRotatesAndPreservesAbsoluteExpiry(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	sid := establish(t, m, h, "alice")

	before, err := mem.Load(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}

	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		// Staged cells land on the renewed SID.
		_ = h.Save(r.Context(), appSession{Name: "renewed"})
		if err := m.Renew(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	newSID, _ := sessionCookie(t, rr)
	if newSID == "" || newSID == sid {
		t.Fatalf("renew did not rotate: %q -> %q", sid, newSID)
	}
	if _, err := mem.Load(context.Background(), sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old SID survived rotation: %v", err)
	}
	after, err := mem.Load(context.Background(), newSID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.AbsoluteExpiry.Equal(before.AbsoluteExpiry) {
		t.Fatalf("rotation moved the hard deadline: %v -> %v", before.AbsoluteExpiry, after.AbsoluteExpiry)
	}
	got, _ := h.Load(context.Background(), newSID)
	if got.Name != "renewed" {
		t.Fatalf("staged cell lost in rotation: %+v", got)
	}
}

func TestRenewSessionlessIsNotFound(t *testing.T) {
	m := newTestManager(t)
	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Renew(r.Context()); !errors.Is(err, ErrNotFound) {
			t.Errorf("sessionless Renew = %v, want ErrNotFound", err)
		}
	})
	// A stale token is sessionless.
	serve(t, m, "dead", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Renew(r.Context()); !errors.Is(err, ErrNotFound) {
			t.Errorf("stale-token Renew = %v, want ErrNotFound", err)
		}
	})
}

func TestPromoteEstablishedRotatesWithIdentity(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	sid := establish(t, m, h, "cart")

	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if err := m.Promote(r.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
		// Promote is visible before commit.
		if uid, _ := m.UserID(r.Context()); uid != "u1" {
			t.Errorf("pre-commit UserID = %q", uid)
		}
	})
	newSID, _ := sessionCookie(t, rr)
	if newSID == sid {
		t.Fatal("Promote kept the pre-login SID - the fixation defense is gone")
	}
	rec, err := mem.Load(context.Background(), newSID)
	if err != nil || rec.UserID != "u1" {
		t.Fatalf("promoted record = %+v, %v", rec, err)
	}
	got, _ := h.Load(context.Background(), newSID)
	if got.Name != "cart" {
		t.Fatalf("login lost the cart: %+v", got)
	}
}

func TestPromoteOnlyLoginMintsAuthenticated(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		// Login does not require session data.
		if err := m.Promote(r.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
	})
	sid, ok := sessionCookie(t, rr)
	if !ok || sid == "" {
		t.Fatal("Promote-only login minted nothing")
	}
	rec, err := mem.Load(context.Background(), sid)
	if err != nil || rec.UserID != "u1" {
		t.Fatalf("minted record = %+v, %v", rec, err)
	}
	if string(rec.Payload) != "{}" {
		t.Fatalf("cellless mint payload = %q, want {}", rec.Payload)
	}
}

func TestDestroyThenSaveMintsFreshEveryField(t *testing.T) {
	mem := NewMemoryStore()
	store := &hookStore{inner: mem}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app
	other, _ := Use(m, NewKey[otherShape]("other"))

	// Use an aged absolute expiry to prove rotation preserves it.
	var sid string
	{
		rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
			_ = h.Save(r.Context(), appSession{Name: "old"})
			_ = other.Save(r.Context(), otherShape{Count: 9})
			_ = m.Promote(r.Context(), "u1")
		})
		sid, _ = sessionCookie(t, rr)
	}
	oldRec, _ := mem.Load(context.Background(), sid)

	store.mu.Lock()
	store.ops = nil
	store.mu.Unlock()

	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_, _ = h.Get(ctx)
		if err := m.Destroy(ctx); err != nil {
			t.Fatal(err)
		}
		// Destroy is visible before commit.
		if uid, _ := m.UserID(ctx); uid != "" {
			t.Errorf("post-Destroy UserID = %q", uid)
		}
		// A later write after Destroy mints a fresh session.
		_ = h.Save(ctx, appSession{Name: "flash"})
	})

	newSID, _ := sessionCookie(t, rr)
	if newSID == "" || newSID == sid {
		t.Fatalf("Destroy-then-Save SID: %q -> %q", sid, newSID)
	}
	if _, err := mem.Load(context.Background(), sid); !errors.Is(err, ErrNotFound) {
		t.Fatal("destroyed SID survived")
	}

	rec, err := mem.Load(context.Background(), newSID)
	if err != nil {
		t.Fatal(err)
	}
	// The new session is fresh in every field.
	if rec.UserID != "" {
		t.Fatalf("fresh session inherited UserID %q", rec.UserID)
	}
	if !rec.AbsoluteExpiry.After(oldRec.AbsoluteExpiry) {
		t.Fatal("fresh session inherited the old hard deadline")
	}
	if _, ok, _ := m.c.loadCell(context.Background(), newSID, other.Key()); ok {
		t.Fatal("pre-Destroy cell leaked into the fresh session")
	}
	got, _ := h.Load(context.Background(), newSID)
	if got.Name != "flash" {
		t.Fatalf("post-Destroy stage = %+v", got)
	}

	// Destroy deletes the old record before inserting the new one.
	var deleteIdx, saveIdx = -1, -1
	for i, op := range store.opLog() {
		if op == "delete:"+sid && deleteIdx < 0 {
			deleteIdx = i
		}
		if op == "save:"+newSID && saveIdx < 0 {
			saveIdx = i
		}
	}
	if deleteIdx < 0 || saveIdx < 0 || deleteIdx > saveIdx {
		t.Fatalf("destroy-commit ordering: %v", store.opLog())
	}
}

func TestDestroyFailedDeleteFailsCommit(t *testing.T) {
	mem := NewMemoryStore()
	store := &hookStore{inner: mem}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app
	sid := establish(t, m, h, "v")

	store.beforeDelete = func(string) error { return errors.New("store down") }
	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		_ = m.Destroy(r.Context())
		_ = h.Save(r.Context(), appSession{Name: "flash"})
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("failed delete = %d, want 500", rr.Code)
	}
	// A failed delete prevents the replacement session.
	if v, ok := sessionCookie(t, rr); ok && v != "" {
		t.Fatalf("failed logout still emitted a token: %q", v)
	}
}

func TestDestroyStaleAndCleanLogout(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app

	// Destroy on a stale token is idempotent.
	serve(t, m, "dead", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Destroy(r.Context()); err != nil {
			t.Errorf("stale Destroy = %v", err)
		}
	})

	// Plain logout deletes the record and clears the token.
	sid := establish(t, m, h, "v")
	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		if err := m.Destroy(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := mem.Load(context.Background(), sid); !errors.Is(err, ErrNotFound) {
		t.Fatal("logout left the record")
	}
	if v, ok := sessionCookie(t, rr); !ok || v != "" {
		t.Fatalf("logout token = (%q, %v), want a clear", v, ok)
	}
}

func TestPostCommitMutatorMatrix(t *testing.T) {
	m := newTestManager(t)
	h := m.app
	sid := establish(t, m, h, "v")

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_, _ = h.Get(ctx)
		w.WriteHeader(http.StatusOK) // response starts

		for name, err := range map[string]error{
			"Save":    h.Save(ctx, appSession{Name: "x"}),
			"Clear":   h.Clear(ctx),
			"Renew":   m.Renew(ctx),
			"Promote": m.Promote(ctx, "u"),
			"Destroy": m.Destroy(ctx),
		} {
			if !errors.Is(err, ErrAlreadyCommitted) {
				t.Errorf("post-commit %s = %v, want ErrAlreadyCommitted", name, err)
			}
		}
		// Established-session Update still works after response start.
		if err := h.Update(ctx, func(s *appSession) error {
			s.Name = "streamed"
			return nil
		}); err != nil {
			t.Errorf("post-commit established Update = %v", err)
		}
	})
	got, _ := h.Load(context.Background(), sid)
	if got.Name != "streamed" {
		t.Fatalf("post-commit Update value = %+v", got)
	}

	// Staged mutators error after response start, even for no-ops.
	fresh := newTestManager(t)
	hf := fresh.app
	serve(t, fresh, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if err := hf.Clear(r.Context()); !errors.Is(err, ErrAlreadyCommitted) {
			t.Errorf("post-commit no-op Clear = %v, want ErrAlreadyCommitted", err)
		}
		// A minting Update cannot send its SID after response start.
		if err := hf.Update(r.Context(), func(s *appSession) error { return nil }); !errors.Is(err, ErrAlreadyCommitted) {
			t.Errorf("post-commit minting Update = %v, want ErrAlreadyCommitted", err)
		}
	})
}

func TestFlushIsResponseStart(t *testing.T) {
	m := newTestManager(t)
	h := m.app
	sid := establish(t, m, h, "v")

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		w.(http.Flusher).Flush()
		if err := h.Save(r.Context(), appSession{Name: "late"}); !errors.Is(err, ErrAlreadyCommitted) {
			t.Errorf("Save after Flush = %v, want ErrAlreadyCommitted", err)
		}
	})
}

func TestWriterAdvertisesOnlySupportedInterfaces(t *testing.T) {
	commit := func(http.ResponseWriter) error { return nil }

	bare := struct{ http.ResponseWriter }{}
	w := wrapWriter(&committingWriter{ResponseWriter: bare, commitFn: commit})
	if _, ok := w.(http.Flusher); ok {
		t.Error("wrapper claims Flusher over a bare writer - it lies to ResponseController")
	}
	if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
		t.Error("wrapper does not expose Unwrap")
	}

	flusher := struct {
		http.ResponseWriter
		http.Flusher
	}{}
	w = wrapWriter(&committingWriter{ResponseWriter: flusher, commitFn: commit})
	if _, ok := w.(http.Flusher); !ok {
		t.Error("Flusher-capable underlying not advertised")
	}
	if _, ok := w.(http.Hijacker); ok {
		t.Error("wrapper claims Hijacker over a non-hijackable writer")
	}
}

func TestCorruptCellOperationMatrix(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	other, _ := Use(m, NewKey[otherShape]("other"))
	sid := establish(t, m, h, "fine")

	// Corrupt one cell's value directly in the store.
	rec, _ := mem.Load(context.Background(), sid)
	rec.Payload = []byte(`{"` + h.Key() + `": not-json, "other": {"Count": 3}}`)
	// Build a valid envelope with one corrupt cell value.
	rec.Payload = []byte(`{"` + h.Key() + `": "not-an-object", "other": {"Count": 3}}`)
	if _, err := mem.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// Get and Update return the decode error.
		if _, err := h.Get(ctx); err == nil {
			t.Error("Get on corrupt cell succeeded")
		}
		if err := h.Update(ctx, func(*appSession) error { return nil }); err == nil {
			t.Error("Update on corrupt cell succeeded")
		}
		// Has reports existence, not validity.
		if ok, err := h.Has(ctx); err != nil || !ok {
			t.Errorf("Has on corrupt cell = (%v, %v)", ok, err)
		}
		// The corruption is cell-isolated.
		if v, err := other.Get(ctx); err != nil || v.Count != 3 {
			t.Errorf("sibling cell = %+v, %v", v, err)
		}
		// Save overwrites without decoding.
		if err := h.Save(ctx, appSession{Name: "recovered"}); err != nil {
			t.Errorf("Save on corrupt cell = %v", err)
		}
	})
	got, err := h.Load(context.Background(), sid)
	if err != nil || got.Name != "recovered" {
		t.Fatalf("recovery = %+v, %v", got, err)
	}
}

func TestMalformedEnvelopeMatrix(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	sid := establish(t, m, h, "fine")

	rec, _ := mem.Load(context.Background(), sid)
	rec.UserID = "u1"
	rec.Payload = []byte(`[1,2,3]`)
	if _, err := mem.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// Every cell operation returns the session-level error.
		if _, err := h.Get(ctx); err == nil {
			t.Error("Get on malformed envelope succeeded")
		}
		if _, err := h.Has(ctx); err == nil {
			t.Error("Has on malformed envelope succeeded")
		}
		if err := h.Save(ctx, appSession{Name: "x"}); err == nil {
			t.Error("Save on malformed envelope succeeded")
		}
		if err := h.Clear(ctx); err == nil {
			t.Error("Clear on malformed envelope succeeded")
		}
		// SID and UserID read metadata, not payload.
		if s, err := m.SID(ctx); err != nil || s != sid {
			t.Errorf("SID on malformed payload = (%q, %v)", s, err)
		}
		if uid, err := m.UserID(ctx); err != nil || uid != "u1" {
			t.Errorf("UserID on malformed payload = (%q, %v)", uid, err)
		}
		// Renew preserves malformed payload bytes.
		if err := m.Renew(ctx); err != nil {
			t.Errorf("Renew on malformed payload = %v", err)
		}
	})
	newSID, _ := sessionCookie(t, rr)
	rotated, err := mem.Load(context.Background(), newSID)
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated.Payload) != `[1,2,3]` {
		t.Fatalf("rotation rewrote malformed payload: %q", rotated.Payload)
	}
	if rotated.UserID != "u1" {
		t.Fatalf("rotation lost identity: %q", rotated.UserID)
	}
}

func TestDestroyNeverDecodesEnvelope(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	sid := establish(t, m, h, "fine")

	rec, _ := mem.Load(context.Background(), sid)
	rec.Payload = []byte(`garbage`)
	if _, err := mem.Save(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if err := m.Destroy(r.Context()); err != nil {
			t.Errorf("Destroy on malformed payload = %v", err)
		}
	})
	if _, err := mem.Load(context.Background(), sid); !errors.Is(err, ErrNotFound) {
		t.Fatal("malformed session survived Destroy")
	}
	if v, ok := sessionCookie(t, rr); !ok || v != "" {
		t.Fatal("Destroy did not clear the token")
	}
}

func TestOutOfBandTrioAndAbsence(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	other, _ := Use(m, NewKey[otherShape]("other"))
	sid := establish(t, m, h, "alice")
	ctx := context.Background()

	// Out-of-band operations do not create missing sessions.
	if _, err := h.Load(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load missing = %v", err)
	}
	if err := h.UpdateSID(ctx, "missing", func(*appSession) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateSID missing = %v", err)
	}
	if err := h.ClearSID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClearSID missing = %v", err)
	}
	if _, err := mem.Load(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Error("out-of-band op minted a session")
	}

	// Absent cells read as zero values.
	if v, err := other.Load(ctx, sid); err != nil || v.Count != 0 {
		t.Errorf("Load absent cell = %+v, %v", v, err)
	}
	if err := other.UpdateSID(ctx, sid, func(s *otherShape) error {
		if s.Count != 0 {
			t.Errorf("UpdateSID base = %+v, want zero", s)
		}
		s.Count = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := other.ClearSID(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if ok, _ := hasOutOfBand(m, sid, other.Key()); ok {
		t.Error("ClearSID left the cell")
	}

	// Out-of-band operations do not bump idle TTL.
	before, _ := mem.Load(ctx, sid)
	time.Sleep(2 * time.Millisecond)
	_ = h.UpdateSID(ctx, sid, func(s *appSession) error { s.Name = "job"; return nil })
	after, _ := mem.Load(ctx, sid)
	if after.IdleExpiry.After(before.IdleExpiry) {
		t.Error("out-of-band update slid the idle expiry")
	}

	// DestroySID is idempotent.
	if err := m.DestroySID(ctx, "missing"); err != nil {
		t.Errorf("DestroySID missing = %v", err)
	}
	if err := m.DestroySID(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Load(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Fatal("DestroySID left the record")
	}
}

func hasOutOfBand(m *Manager[appSession], sid, key string) (bool, error) {
	_, ok, err := m.c.loadCell(context.Background(), sid, key)
	return ok, err
}

func TestCapabilityMissing(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.Store = plainStore{inner: NewMemoryStore()} })
	if _, err := m.ListForUser(context.Background(), "u"); !errors.Is(err, ErrCapabilityMissing) {
		t.Errorf("ListForUser = %v", err)
	}
	if _, err := m.RevokeAllForUser(context.Background(), "u"); !errors.Is(err, ErrCapabilityMissing) {
		t.Errorf("RevokeAllForUser = %v", err)
	}
}

func TestReadOnlyBumpTokenRules(t *testing.T) {
	clock := time.Now()
	now := func() time.Time { return clock }
	mem := NewMemoryStore()
	mem.now = now
	m := newTestManager(t, func(c *Config) {
		c.Store = mem
		c.Now = now
		c.IdleExpiry = 10 * time.Minute
		c.IdleBumpInterval = time.Minute
	})
	h := m.app
	sid := establish(t, m, h, "v")

	// Within the bump interval: throttled, no token.
	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
	})
	if _, ok := sessionCookie(t, rr); ok {
		t.Fatal("throttled bump emitted a token")
	}

	// Past the interval, the bump refreshes the token.
	clock = clock.Add(2 * time.Minute)
	rr = serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
	})
	val, ok := sessionCookie(t, rr)
	if !ok || val != sid {
		t.Fatalf("bump token = (%q, %v), want re-emission of %q", val, ok, sid)
	}
	rec, _ := mem.Load(context.Background(), sid)
	if !rec.IdleExpiry.Equal(clock.Add(10 * time.Minute)) {
		t.Fatalf("idle expiry = %v", rec.IdleExpiry)
	}

	// Stores without TTLBumper skip read-time sliding.
	plain := newTestManager(t, func(c *Config) {
		c.Store = plainStore{inner: NewMemoryStore()}
		c.Now = now
		c.IdleExpiry = 10 * time.Minute
		c.IdleBumpInterval = time.Minute
	})
	hp := plain.app
	psid := establish(t, plain, hp, "v")
	clock = clock.Add(5 * time.Minute)
	rr = serve(t, plain, psid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = hp.Get(r.Context())
	})
	if _, ok := sessionCookie(t, rr); ok {
		t.Fatal("TTLBumper-absent read emitted a token")
	}

	// Failed bumps do not refresh the client deadline.
	failing := newTestManager(t, func(c *Config) {
		c.Store = &bumpFailStore{MemoryStore: mem}
		c.Now = now
		c.IdleExpiry = 10 * time.Minute
		c.IdleBumpInterval = time.Minute
	})
	hfail := failing.app
	clock = clock.Add(2 * time.Minute) // past the interval, record still live
	rr = serve(t, failing, sid, func(w http.ResponseWriter, r *http.Request) {
		if _, err := hfail.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if _, ok := sessionCookie(t, rr); ok {
		t.Fatal("failed bump emitted a token")
	}
}

// bumpFailStore fails every TTL bump while delegating everything else.
type bumpFailStore struct{ *MemoryStore }

func (b *bumpFailStore) BumpTTL(context.Context, string, time.Time) error {
	return errors.New("bump failed")
}

func TestTokenExpiryMinRule(t *testing.T) {
	abs := time.Now().Add(time.Hour)
	idle := time.Now().Add(10 * time.Minute)

	if got := tokenExpiry(Record{AbsoluteExpiry: abs, IdleExpiry: idle}); !got.Equal(idle) {
		t.Errorf("min = %v, want idle", got)
	}
	// Disabled idle expiry drops out of the min.
	if got := tokenExpiry(Record{AbsoluteExpiry: abs}); !got.Equal(abs) {
		t.Errorf("disabled idle min = %v, want absolute", got)
	}
}

func TestPostCommitUpdateMovesServerSideOnly(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	sid := establish(t, m, h, "v")

	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		_, _ = h.Get(r.Context())
		w.WriteHeader(http.StatusOK)
		if err := h.Update(r.Context(), func(s *appSession) error {
			s.Name = "mid-stream"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	// Post-start Update changes state without token emission.
	body := rr.Body.String()
	if strings.Contains(body, "session commit failed") {
		t.Fatalf("commit failed: %s", body)
	}
	cookies := rr.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "sid" && c.Value != "" && c.MaxAge > 0 {
			// Any sid cookie here came from the pre-write commit.
			t.Fatalf("post-commit Update refreshed the client token: %v", c)
		}
	}
	got, _ := h.Load(context.Background(), sid)
	if got.Name != "mid-stream" {
		t.Fatalf("post-commit Update value = %+v", got)
	}
}

func TestUpdateThenStagedSaveVersionCoherence(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store; c.MaxRetries = -1 })
	ha := m.app
	hb, _ := Use(m, NewKey[otherShape]("other"))
	sid := establish(t, m, ha, "v")

	// Immediate Update refreshes the request state's CAS version.
	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if err := ha.Update(ctx, func(s *appSession) error {
			s.Name = "updated"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_ = hb.Save(ctx, otherShape{Count: 5})
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("version coherence: commit = %d, body %q", rr.Code, rr.Body.String())
	}
	a, _ := ha.Load(context.Background(), sid)
	b, _ := hb.Load(context.Background(), sid)
	if a.Name != "updated" || b.Count != 5 {
		t.Fatalf("cross-cell convergence: %+v, %+v", a, b)
	}
}

func TestDestroyThenPromoteVisibility(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	sid := establish(t, m, h, "v")
	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_, _ = h.Get(ctx)
		_ = m.Promote(ctx, "old-user")
		_ = m.Destroy(ctx)
		if uid, _ := m.UserID(ctx); uid != "" {
			t.Errorf("post-Destroy UserID = %q", uid)
		}
		_ = m.Promote(ctx, "new-user")
		if uid, _ := m.UserID(ctx); uid != "new-user" {
			t.Errorf("Destroy-then-Promote UserID = %q", uid)
		}
	})
}

func TestCommitFailureDiscardsHandlerBody(t *testing.T) {
	mem := NewMemoryStore()
	store := &hookStore{inner: mem}
	m := newTestManager(t, func(c *Config) { c.Store = store; c.MaxRetries = -1 })
	h := m.app

	store.beforeSave = func(Record) error { return errors.New("store down") }
	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := h.Save(r.Context(), appSession{Name: "v"}); err != nil {
			t.Fatal(err)
		}
		n, err := w.Write([]byte("SHOULD-NOT-APPEAR"))
		if err != nil || n != len("SHOULD-NOT-APPEAR") {
			t.Errorf("post-failure write reported (%d, %v)", n, err)
		}
	})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("commit failure = %d, want 500", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "session commit failed") {
		t.Fatalf("500 body: %q", body)
	}
	if strings.Contains(body, "SHOULD-NOT-APPEAR") {
		t.Fatalf("handler body leaked into the failure response: %q", body)
	}
}

func TestPostCommitSaveOutranksEncodeError(t *testing.T) {
	m := newTestManager(t)
	h, _ := Use(m, NewKey[struct{ Ch chan int }]("unencodable"))

	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		err := h.Save(r.Context(), struct{ Ch chan int }{Ch: make(chan int)})
		if !errors.Is(err, ErrAlreadyCommitted) {
			t.Errorf("post-commit unencodable Save = %v, want ErrAlreadyCommitted (never data-dependent)", err)
		}
	})
}

func TestWriterAdvertisementIsExhaustive(t *testing.T) {
	commit := func(http.ResponseWriter) error { return nil }
	type rfOnly struct {
		http.ResponseWriter
		io.ReaderFrom
	}
	type hjOnly struct {
		http.ResponseWriter
		http.Hijacker
	}
	type fpr struct {
		http.ResponseWriter
		http.Flusher
		http.Pusher
		io.ReaderFrom
	}

	w := wrapWriter(&committingWriter{ResponseWriter: rfOnly{}, commitFn: commit})
	if _, ok := w.(io.ReaderFrom); !ok {
		t.Error("ReaderFrom-only underlying lost ReadFrom")
	}
	if _, ok := w.(http.Flusher); ok {
		t.Error("ReaderFrom-only underlying gained Flusher")
	}

	w = wrapWriter(&committingWriter{ResponseWriter: hjOnly{}, commitFn: commit})
	if _, ok := w.(http.Hijacker); !ok {
		t.Error("Hijacker-without-Flusher lost Hijack")
	}

	w = wrapWriter(&committingWriter{ResponseWriter: fpr{}, commitFn: commit})
	if _, ok := w.(io.ReaderFrom); !ok {
		t.Error("Flusher+Pusher+ReaderFrom lost ReadFrom")
	}
	if _, ok := w.(http.Pusher); !ok {
		t.Error("Flusher+Pusher+ReaderFrom lost Push")
	}
	if _, ok := w.(http.Hijacker); ok {
		t.Error("Flusher+Pusher+ReaderFrom gained Hijacker")
	}
}

func TestMultiTokenValidation(t *testing.T) {
	cfg := testConfig()
	cfg.Token = Multi{}
	if _, err := New[appSession](cfg); err == nil || !strings.Contains(err.Error(), "no members") {
		t.Errorf("empty Multi accepted: %v", err)
	}
	cfg.Token = Multi{Cookie{}, nil}
	if _, err := New[appSession](cfg); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Errorf("nil Multi member accepted: %v", err)
	}
	cfg.Token = Multi{Cookie{}, Bearer{}}
	if _, err := New[appSession](cfg); err != nil {
		t.Errorf("valid Multi rejected: %v", err)
	}
	cfg.Token = Multi{Multi{Cookie{}}, Multi{}}
	if _, err := New[appSession](cfg); err == nil {
		t.Error("nested empty Multi accepted")
	}
}

// A panicking Update closure leaves the request state usable.
func TestUpdatePanicIsRecoverable(t *testing.T) {
	m := newTestManager(t)
	h := m.app
	sid := establish(t, m, h, "v")

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		func() {
			defer func() {
				if recover() == nil {
					t.Error("closure panic was swallowed")
				}
			}()
			_ = h.Update(ctx, func(s *appSession) error { panic("app bug") })
		}()
		// The state is still usable.
		if v, err := h.Get(ctx); err != nil || v.Name != "v" {
			t.Errorf("state after recovered panic = %+v, %v", v, err)
		}
	})
}

// Request cancellation does not cancel staged store operations.
func TestCommitSurvivesCanceledRequestContext(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	h := m.app
	sid := establish(t, m, h, "v")

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	cctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(cctx)
	rr := httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Destroy(r.Context()); err != nil {
			t.Fatal(err)
		}
		cancel() // the client disconnects before response start
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("commit under canceled context = %d, body %q", rr.Code, rr.Body.String())
	}
	if _, err := mem.Load(context.Background(), sid); !errors.Is(err, ErrNotFound) {
		t.Fatal("logout was lost to request-context cancellation - the session survived")
	}
}

// hijackRecorder implements Hijacker over a recorder using an
// in-memory pipe.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, _ := net.Pipe()
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}

// Hijack after a write follows net/http behavior.
func TestHijackAfterWriteIsAllowed(t *testing.T) {
	m := newTestManager(t)
	h := m.app
	sid := establish(t, m, h, "v")

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	hr := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatalf("hijack after WriteHeader = %v, want success", err)
		}
		conn.Close()
	})).ServeHTTP(hr, req)
	if !hr.hijacked {
		t.Fatal("underlying connection was never hijacked")
	}
}

// A failed commit makes the connection non-hijackable.
func TestHijackRefusedAfterFailedCommit(t *testing.T) {
	store := &hookStore{inner: NewMemoryStore()}
	m := newTestManager(t, func(c *Config) { c.Store = store; c.MaxRetries = -1 })
	h := m.app

	store.beforeSave = func(Record) error { return errors.New("store down") }
	req := httptest.NewRequest("GET", "/", nil)
	hr := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = h.Save(r.Context(), appSession{Name: "v"})
		if _, _, err := w.(http.Hijacker).Hijack(); !errors.Is(err, http.ErrHijacked) {
			t.Fatalf("hijack after failed commit = %v, want ErrHijacked", err)
		}
	})).ServeHTTP(hr, req)
	if hr.hijacked {
		t.Fatal("connection handed over despite the shipped 500")
	}
}

// Destroy on a cookie-less fresh visitor: sessionless is
// sessionless, whatever the reason - idempotent success, nothing
// deleted, no token instruction at all.
func TestDestroyCookielessVisitor(t *testing.T) {
	m := newTestManager(t)
	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Destroy(r.Context()); err != nil {
			t.Errorf("cookie-less Destroy = %v", err)
		}
	})
	if _, ok := sessionCookie(t, rr); ok {
		t.Error("cookie-less Destroy emitted a token instruction")
	}
}

// Lifecycle is sealed to Manager values and composes with Registry
// by embedding, the way a library declares exactly the capabilities
// it exercises.
func TestLifecycleSealedCapability(t *testing.T) {
	m := newTestManager(t)
	var l Lifecycle = m
	if l.lifecycle() != m.c {
		t.Fatal("Lifecycle view does not anchor the same engine")
	}
	var both interface {
		Registry
		Lifecycle
	} = m
	if both.registry() != both.lifecycle() {
		t.Fatal("composed capability views diverge")
	}
}

// A privilege change must not silently leave the previous
// identity's SID alive: Promote's rotation delete failing fails the
// commit - no token, request errors, prior session intact, and the
// new row sits unreferenced until it expires.
// Re-authenticating an already-authenticated session rotates a SID
// that carried a real identity. If deleting that old SID fails, the
// commit must fail: leaving it alive would keep the prior identity
// reachable on a stolen token.
func TestPromoteDeleteFailureFailsCommit(t *testing.T) {
	mem := NewMemoryStore()
	store := &hookStore{inner: mem}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app
	sid0 := establish(t, m, h, "cart")

	// First login: anonymous -> u1. Succeeds and rotates to sid1.
	rr1 := serve(t, m, sid0, func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Promote(r.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
	})
	if rr1.Code != http.StatusOK {
		t.Fatalf("first login = %d, want 200", rr1.Code)
	}
	sid1, ok := sessionCookie(t, rr1)
	if !ok || sid1 == "" {
		t.Fatalf("first login issued no token")
	}

	// Re-login on the now-authenticated session with the old-SID
	// delete failing: sid1 held u1, so the commit must fail.
	store.beforeDelete = func(string) error { return errors.New("store down") }
	rr2 := serve(t, m, sid1, func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Promote(r.Context(), "u2"); err != nil {
			t.Fatal(err)
		}
	})
	if rr2.Code != http.StatusInternalServerError {
		t.Fatalf("re-promote with failed delete = %d, want 500", rr2.Code)
	}
	if v, ok := sessionCookie(t, rr2); ok && v != "" {
		t.Fatalf("failed privilege change still issued a token: %q", v)
	}
	// The prior authenticated session is intact under sid1.
	rec, err := mem.Load(context.Background(), sid1)
	if err != nil || rec.UserID != "u1" {
		t.Fatalf("prior session damaged: %+v, %v", rec, err)
	}
	// The residue: one unreferenced row (the new sid2), expiring.
	var orphans int
	_ = mem.Scan(context.Background(), func(s string) bool {
		if s != sid1 {
			orphans++
		}
		return true
	})
	if orphans != 1 {
		t.Fatalf("residue = %d orphaned rows, want 1", orphans)
	}
}

// A fresh login promotes an anonymous session. If deleting that old
// anonymous SID fails, the survivor holds no authenticated identity,
// so the login still commits and issues its token - best-effort, like
// renew.
func TestPromoteFromAnonymousDeleteFailureStaysBestEffort(t *testing.T) {
	mem := NewMemoryStore()
	store := &hookStore{inner: mem}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app
	sid := establish(t, m, h, "cart")

	store.beforeDelete = func(string) error { return errors.New("store down") }
	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Promote(r.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("fresh login with failed delete = %d, want 200", rr.Code)
	}
	newSID, ok := sessionCookie(t, rr)
	if !ok || newSID == "" || newSID == sid {
		t.Fatalf("login token = (%q, %v), want a fresh SID", newSID, ok)
	}
	// The old anonymous row survives, harmless.
	rec, err := mem.Load(context.Background(), sid)
	if err != nil || rec.UserID != "" {
		t.Fatalf("stale anonymous row: %+v, %v", rec, err)
	}
	// The new row carries the authenticated identity.
	nrec, err := mem.Load(context.Background(), newSID)
	if err != nil || nrec.UserID != "u1" {
		t.Fatalf("new session not authenticated: %+v, %v", nrec, err)
	}
}

// Renew's delete stays best-effort: same identity, harmless stale
// survivor - the rotation succeeds and the new token goes out.
func TestRenewDeleteFailureStaysBestEffort(t *testing.T) {
	mem := NewMemoryStore()
	store := &hookStore{inner: mem}
	m := newTestManager(t, func(c *Config) { c.Store = store })
	h := m.app
	sid := establish(t, m, h, "v")

	store.beforeDelete = func(string) error { return errors.New("store down") }
	rr := serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if _, err := h.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Renew(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("renew with failed delete = %d, want 200", rr.Code)
	}
	newSID, ok := sessionCookie(t, rr)
	if !ok || newSID == "" || newSID == sid {
		t.Fatalf("renew token = (%q, %v), want a fresh SID", newSID, ok)
	}
	// The stale survivor is present and harmless.
	if _, err := mem.Load(context.Background(), sid); err != nil {
		t.Fatalf("stale row should survive best-effort delete: %v", err)
	}
}
