package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// App data uses the reserved app cell beside library cells.
func TestAppTierReservedCellAndCoexistence(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	lib, err := Use[otherShape](m, "github.com/example/lib")
	if err != nil {
		t.Fatal(err)
	}

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if err := m.Save(ctx, appSession{Name: "alice"}); err != nil {
			t.Fatal(err)
		}
		if err := lib.Save(ctx, otherShape{Count: 7}); err != nil {
			t.Fatal(err)
		}
	})
	sid, ok := sessionCookie(t, rr)
	if !ok || sid == "" {
		t.Fatal("no session minted")
	}

	rec, err := mem.Load(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	var cells map[string]json.RawMessage
	if err := json.Unmarshal(rec.Payload, &cells); err != nil {
		t.Fatal(err)
	}
	if _, ok := cells["app"]; !ok {
		t.Fatalf("app data not under the reserved key: %s", rec.Payload)
	}
	if _, ok := cells["github.com/example/lib"]; !ok {
		t.Fatalf("library cell missing: %s", rec.Payload)
	}

	// Each tier reads its own data back.
	if got, err := m.Load[appSession](context.Background(), sid); err != nil || got.Name != "alice" {
		t.Fatalf("app Load = %+v, %v", got, err)
	}
	if got, err := lib.Load(context.Background(), sid); err != nil || got.Count != 7 {
		t.Fatalf("library Load = %+v, %v", got, err)
	}
}

// The app facade exposes the same lifecycle engine.
func TestAppTierLifecycleDelegation(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })

	rr := serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Promote(r.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
	})
	sid, _ := sessionCookie(t, rr)
	rec, err := mem.Load(context.Background(), sid)
	if err != nil || rec.UserID != "u1" {
		t.Fatalf("Promote through the facade: %+v, %v", rec, err)
	}

	serve(t, m, sid, func(w http.ResponseWriter, r *http.Request) {
		if _, err := m.Get[appSession](r.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Destroy(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := m.Load[appSession](context.Background(), sid); err == nil {
		t.Fatal("Destroy through the facade left the record")
	}
}

// Reads pin the app type.
func TestPinOnRead(t *testing.T) {
	m := newTestManager(t)
	serve(t, m, "", func(w http.ResponseWriter, r *http.Request) {
		if _, err := m.Get[appSession](r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := m.Get[otherShape](context.Background()); err == nil ||
		!strings.Contains(err.Error(), "already registered") {
		t.Fatalf("conflicting read after pin-by-read: want registry conflict, got %v", err)
	}
}

// Pin errors precede request and store errors on every typed accessor.
func TestPinErrorPrecedesNoSession(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.Store = failingStore{} })
	ctx := context.Background()
	if err := m.Save(ctx, appSession{Name: "a"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("valid pin without middleware: want ErrNoSession, got %v", err)
	}
	checkConflict := func(op string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, ErrNoSession) ||
			!strings.Contains(err.Error(), "already registered") {
			t.Fatalf("%s with conflicting type: want registry conflict first, got %v", op, err)
		}
	}
	_, err := m.Get[otherShape](ctx)
	checkConflict("Get", err)
	checkConflict("Save", m.Save(ctx, otherShape{Count: 1}))
	checkConflict("Update", m.Update(ctx, func(*otherShape) error { return nil }))
	_, err = m.Load[otherShape](ctx, "sid")
	checkConflict("Load", err)
	checkConflict("UpdateSID", m.UpdateSID(ctx, "sid", func(*otherShape) error { return nil }))
}

type failingStore struct{}

func (failingStore) Load(context.Context, string) (Record, error) {
	return Record{}, errors.New("store down")
}
func (failingStore) Save(context.Context, Record) (Record, error) {
	return Record{}, errors.New("store down")
}
func (failingStore) Delete(context.Context, string) error { return errors.New("store down") }

// Exactly one type wins a concurrent first pin.
func TestConcurrentPinSingleWinner(t *testing.T) {
	m := newTestManager(t)
	var wg sync.WaitGroup
	const perType = 16
	errs := make([]error, perType*2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				errs[i] = pinApp[appSession](m)
			} else {
				errs[i] = pinApp[otherShape](m)
			}
		}(i)
	}
	wg.Wait()
	var appErrs, otherErrs int64
	for i, err := range errs {
		if err == nil {
			continue
		}
		if !strings.Contains(err.Error(), "already registered") {
			t.Fatalf("loser error is not the registry conflict: %v", err)
		}
		if i%2 == 0 {
			appErrs++
		} else {
			otherErrs++
		}
	}
	p := m.appT.Load()
	if p == nil {
		t.Fatal("nothing pinned")
	}
	winErrs, loseErrs := appErrs, otherErrs
	winnerIsApp := *p == reflect.TypeFor[appSession]()
	if !winnerIsApp {
		winErrs, loseErrs = otherErrs, appErrs
	}
	if got := m.cells[appKey]; got != *p {
		t.Fatalf("registry holds %s, pinned type is %s", got, *p)
	}
	for range 4 {
		var winErr, loseErr error
		if winnerIsApp {
			winErr, loseErr = pinApp[appSession](m), pinApp[otherShape](m)
		} else {
			winErr, loseErr = pinApp[otherShape](m), pinApp[appSession](m)
		}
		if winErr != nil {
			t.Fatalf("post-pin access with the winning type failed: %v", winErr)
		}
		if loseErr == nil || !strings.Contains(loseErr.Error(), "already registered") {
			t.Fatalf("post-pin losing type: want registry conflict, got %v", loseErr)
		}
	}
	if winErrs != 0 || loseErrs != perType {
		t.Fatalf("winner errors = %d (want 0), loser errors = %d (want %d)", winErrs, loseErrs, perType)
	}
}
