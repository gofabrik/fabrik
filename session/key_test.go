package session

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type appSession struct{ Name string }

type otherShape struct{ Count int }

type box[T any] struct{ V T }

func testConfig() Config {
	return Config{
		Store:          NewMemoryStore(),
		Token:          Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
	}
}

func newTestManager(t *testing.T, mutate ...func(*Config)) *Manager {
	t.Helper()
	cfg := testConfig()
	for _, fn := range mutate {
		fn(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewKeyPanics(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		fn()
	}
	mustPanic("empty name", func() { NewKey[appSession]("") })
	mustPanic("reserved name", func() { NewKey[appSession]("app") })
}

func TestTypedAccessRejectsNonStruct(t *testing.T) {
	// Type errors precede ErrNoSession.
	m := newTestManager(t)
	if _, err := m.Get[int](t.Context()); err == nil || !strings.Contains(err.Error(), "app-cell access") {
		t.Errorf("int session type: want shape error before ErrNoSession, got %v", err)
	}
	if err := m.Save(t.Context(), map[string]any{}); err == nil || !strings.Contains(err.Error(), "app-cell access") {
		t.Errorf("map session type: want shape error before ErrNoSession, got %v", err)
	}
}

func TestUseTypeChecks(t *testing.T) {
	m := newTestManager(t)
	if _, err := Use(m, Key[map[string]any]{name: "m"}); err == nil {
		t.Error("map accepted as cell type")
	}
	if _, err := Use(m, Key[appSession]{}); err == nil {
		t.Error("zero Key accepted")
	}
	// Keys provide identity; types provide shape.
	if _, err := Use(m, NewKey[box[int]]("boxed")); err != nil {
		t.Errorf("generic instantiation rejected: %v", err)
	}
	if _, err := Use(m, NewKey[struct{ N int }]("anon")); err != nil {
		t.Errorf("anonymous struct rejected: %v", err)
	}
}

func TestKeyRegistryMatrix(t *testing.T) {
	m := newTestManager(t)

	// The same Key value is idempotent.
	key := NewKey[otherShape]("github.com/example/lib")
	h1, err := Use(m, key)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Use(m, key)
	if err != nil {
		t.Fatalf("idempotent re-registration: %v", err)
	}
	if h1.Key() != h2.Key() {
		t.Fatalf("same key, different cells: %q vs %q", h1.Key(), h2.Key())
	}

	// The same name from a different type is a registration error.
	if _, err := Use(m, NewKey[appSession]("github.com/example/lib")); err == nil {
		t.Error("same name accepted for a different type")
	}

	// Different names are distinct cells regardless of type.
	a, err := Use(m, NewKey[otherShape]("draft"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Key() == h1.Key() {
		t.Fatal("distinct names collapsed")
	}

	if _, err := Use(m, Key[otherShape]{name: "app"}); err == nil {
		t.Error("library key collided with the app cell silently")
	}
	if _, err := Use(m, Key[appSession]{name: "app"}); err == nil {
		t.Error("forged same-type app key accepted")
	}
}

func TestManagerSatisfiesRegistry(t *testing.T) {
	m := newTestManager(t)
	var r Registry = m
	if r.registry() != m {
		t.Fatal("Registry view does not anchor the same engine")
	}
}

func TestConcurrentFirstRegistration(t *testing.T) {
	m := newTestManager(t)
	key := NewKey[otherShape]("github.com/example/concurrent")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Use(m, key)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	base := testConfig()

	broken := base
	broken.Store = nil
	broken.Token = nil
	broken.AbsoluteExpiry = 0
	if _, err := New(broken); err == nil {
		t.Fatal("empty config accepted")
	} else {
		for _, want := range []string{"Store is required", "Token is required", "AbsoluteExpiry"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("joined validation missing %q: %v", want, err)
			}
		}
	}

	neg := base
	neg.IdleExpiry = -time.Second
	if _, err := New(neg); err == nil {
		t.Error("negative IdleExpiry accepted")
	}

	tooLong := base
	tooLong.IdleExpiry = 2 * base.AbsoluteExpiry
	if _, err := New(tooLong); err == nil {
		t.Error("IdleExpiry exceeding AbsoluteExpiry accepted")
	}

	bumpNoIdle := base
	bumpNoIdle.IdleExpiry = 0
	bumpNoIdle.IdleBumpInterval = time.Minute
	if _, err := New(bumpNoIdle); err == nil {
		t.Error("IdleBumpInterval accepted with idle expiry disabled")
	}

	negBump := base
	negBump.IdleBumpInterval = -time.Second
	if _, err := New(negBump); err == nil {
		t.Error("negative IdleBumpInterval accepted")
	}
}

func TestMaxRetriesSemantics(t *testing.T) {
	// 0 uses the package default; negative disables retries.
	m := newTestManager(t)
	if m.maxRetries != defaultMaxRetries {
		t.Fatalf("zero MaxRetries resolved to %d, want %d", m.maxRetries, defaultMaxRetries)
	}
	m = newTestManager(t, func(c *Config) { c.MaxRetries = -5 })
	if m.maxRetries != 0 {
		t.Fatalf("negative MaxRetries resolved to %d, want 0", m.maxRetries)
	}
	m = newTestManager(t, func(c *Config) { c.MaxRetries = 7 })
	if m.maxRetries != 7 {
		t.Fatalf("MaxRetries resolved to %d, want 7", m.maxRetries)
	}
}

func TestUseRejectsNilRegistry(t *testing.T) {
	key := NewKey[otherShape]("github.com/example/nilcheck")
	if _, err := Use(nil, key); err == nil {
		t.Error("nil Registry accepted")
	}
	var m *Manager // A typed nil still implements Registry.
	if _, err := Use(m, key); err == nil {
		t.Error("typed-nil manager accepted")
	}
}

func appH(m *Manager) *Handle[appSession] {
	if err := pinApp[appSession](m); err != nil {
		panic(err)
	}
	return appHandle[appSession](m)
}
