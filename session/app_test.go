package session

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// App data uses the reserved app cell beside library cells.
func TestAppTierReservedCellAndCoexistence(t *testing.T) {
	mem := NewMemoryStore()
	m := newTestManager(t, func(c *Config) { c.Store = mem })
	lib, err := Use(m, NewKey[otherShape]("github.com/example/lib"))
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
	if got, err := m.Load(context.Background(), sid); err != nil || got.Name != "alice" {
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
		if _, err := m.Get(r.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.Destroy(r.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := m.Load(context.Background(), sid); err == nil {
		t.Fatal("Destroy through the facade left the record")
	}
}
