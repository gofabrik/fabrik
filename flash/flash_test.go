package flash

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/session"
)

type appSession struct{ Name string }

// countingStore tracks session commits.
type countingStore struct {
	session.Store
	mu    sync.Mutex
	saves int
}

func (c *countingStore) Save(ctx context.Context, rec session.Record) (session.Record, error) {
	c.mu.Lock()
	c.saves++
	c.mu.Unlock()
	return c.Store.Save(ctx, rec)
}

func (c *countingStore) saveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saves
}

func harness(t *testing.T) (*session.Manager[appSession], *Flash, *countingStore) {
	t.Helper()
	store := &countingStore{Store: session.NewMemoryStore()}
	m, err := session.New[appSession](session.Config{
		Store:          store,
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	fl, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, fl, store
}

// Use a retry budget high enough for the intentional contention in this file.
func managerWithRetries(t *testing.T, retries int) (*session.Manager[appSession], *Flash) {
	t.Helper()
	m, err := session.New[appSession](session.Config{
		Store:          session.NewMemoryStore(),
		Token:          session.Cookie{},
		AbsoluteExpiry: time.Hour,
		IdleExpiry:     30 * time.Minute,
		MaxRetries:     retries,
	})
	if err != nil {
		t.Fatal(err)
	}
	fl, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, fl
}

// serve runs one request through the session middleware and returns
// the response's session cookie value, if any.
func serve(t *testing.T, m *session.Manager[appSession], sid string, handler func(ctx context.Context)) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	}
	rr := httptest.NewRecorder()
	m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(r.Context())
	})).ServeHTTP(rr, req)
	for _, c := range rr.Result().Cookies() {
		if c.Name == "sid" {
			return c.Value
		}
	}
	return sid
}

func TestAddTakeRoundTrip(t *testing.T) {
	m, fl, _ := harness(t)

	sid := serve(t, m, "", func(ctx context.Context) {
		if err := fl.Add(ctx, "success", "Saved."); err != nil {
			t.Fatal(err)
		}
		if err := fl.Add(ctx, "info", "Check your email."); err != nil {
			t.Fatal(err)
		}
	})
	if sid == "" {
		t.Fatal("flash writes did not mint a session")
	}

	serve(t, m, sid, func(ctx context.Context) {
		msgs, err := fl.Take(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 2 || msgs[0] != (Message{Kind: "success", Text: "Saved."}) || msgs[1].Kind != "info" {
			t.Fatalf("messages = %+v", msgs)
		}
	})

	serve(t, m, sid, func(ctx context.Context) {
		msgs, err := fl.Take(ctx)
		if err != nil || len(msgs) != 0 {
			t.Fatalf("flash survived being taken: %+v, %v", msgs, err)
		}
	})
}

func TestAddMintingNewSessionIsOneWrite(t *testing.T) {
	m, fl, store := harness(t)

	serve(t, m, "", func(ctx context.Context) {
		if err := m.Save(ctx, appSession{Name: "alice"}); err != nil {
			t.Fatal(err)
		}
		if err := fl.Add(ctx, "success", "Saved."); err != nil {
			t.Fatal(err)
		}
	})
	if got := store.saveCount(); got != 1 {
		t.Fatalf("minting app save + flash add produced %d store writes, want 1", got)
	}
}

// Existing sessions commit the app cell and flash cell separately.
func TestAddOnExistingSessionWritesSeparately(t *testing.T) {
	m, fl, store := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = m.Save(ctx, appSession{Name: "alice"})
	})
	before := store.saveCount()

	serve(t, m, sid, func(ctx context.Context) {
		if err := m.Save(ctx, appSession{Name: "bob"}); err != nil {
			t.Fatal(err)
		}
		if err := fl.Add(ctx, "success", "Saved."); err != nil {
			t.Fatal(err)
		}
	})
	if got := store.saveCount() - before; got != 2 {
		t.Fatalf("existing-session app save + flash add produced %d store writes, want 2", got)
	}
}

func TestEmptyTakeStagesNothing(t *testing.T) {
	m, fl, store := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = m.Save(ctx, appSession{Name: "alice"})
	})
	before := store.saveCount()

	serve(t, m, sid, func(ctx context.Context) {
		if msgs, err := fl.Take(ctx); err != nil || len(msgs) != 0 {
			t.Fatalf("empty take = %+v, %v", msgs, err)
		}
	})
	if got := store.saveCount(); got != before {
		t.Fatalf("empty Take wrote to the store: %d -> %d", before, got)
	}
}

func TestPeekDoesNotConsume(t *testing.T) {
	m, fl, _ := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = fl.Add(ctx, "error", "Nope.")
	})

	serve(t, m, sid, func(ctx context.Context) {
		msgs, err := fl.Peek(ctx)
		if err != nil || len(msgs) != 1 {
			t.Fatalf("peek = %+v, %v", msgs, err)
		}
	})
	serve(t, m, sid, func(ctx context.Context) {
		msgs, _ := fl.Take(ctx)
		if len(msgs) != 1 {
			t.Fatalf("peek consumed the message: %+v", msgs)
		}
	})
}

func TestClearDropsWithoutRendering(t *testing.T) {
	m, fl, _ := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = fl.Add(ctx, "info", "Old news.")
	})
	serve(t, m, sid, func(ctx context.Context) {
		if err := fl.Clear(ctx); err != nil {
			t.Fatal(err)
		}
	})
	serve(t, m, sid, func(ctx context.Context) {
		if msgs, _ := fl.Take(ctx); len(msgs) != 0 {
			t.Fatalf("cleared flash still pending: %+v", msgs)
		}
	})
}

// An emptied flash cell remains reusable.
func TestTakeThenReuse(t *testing.T) {
	m, fl, _ := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = fl.Add(ctx, "info", "first")
	})
	serve(t, m, sid, func(ctx context.Context) {
		if msgs, _ := fl.Take(ctx); len(msgs) != 1 || msgs[0].Text != "first" {
			t.Fatalf("first take = %+v", msgs)
		}
	})
	serve(t, m, sid, func(ctx context.Context) {
		_ = fl.Add(ctx, "info", "second")
	})
	serve(t, m, sid, func(ctx context.Context) {
		if msgs, _ := fl.Take(ctx); len(msgs) != 1 || msgs[0].Text != "second" {
			t.Fatalf("second take = %+v", msgs)
		}
	})
}

func TestCoexistsWithAppData(t *testing.T) {
	m, fl, _ := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = m.Save(ctx, appSession{Name: "alice"})
		_ = fl.Add(ctx, "success", "Saved.")
	})
	serve(t, m, sid, func(ctx context.Context) {
		s, err := m.Get(ctx)
		if err != nil || s.Name != "alice" {
			t.Fatalf("app data = %+v, %v", s, err)
		}
		if msgs, _ := fl.Take(ctx); len(msgs) != 1 {
			t.Fatalf("flash lost beside app data: %+v", msgs)
		}
	})
	if !strings.HasPrefix(key.Name(), "github.com/gofabrik/fabrik/flash") {
		t.Fatalf("cell key = %q", key.Name())
	}
}

func TestSequentialAddsAccumulate(t *testing.T) {
	m, fl, _ := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = fl.Add(ctx, "info", "first")
	})
	serve(t, m, sid, func(ctx context.Context) {
		_ = fl.Add(ctx, "info", "second")
	})

	var got []Message
	serve(t, m, sid, func(ctx context.Context) {
		got, _ = fl.Take(ctx)
	})
	if len(got) != 2 {
		t.Fatalf("sequential adds = %+v, want both", got)
	}
}

func TestInterleavedAddsBothSurvive(t *testing.T) {
	m, fl, _ := harness(t)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = m.Save(ctx, appSession{Name: "x"})
	})

	// Request A commits flash, pauses, then resumes after request B commits.
	added := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(t, m, sid, func(ctx context.Context) {
			_ = fl.Add(ctx, "info", "from tab A")
			close(added)
			<-release
		})
	}()
	select {
	case <-added:
	case <-time.After(5 * time.Second):
		t.Fatal("request A never added its flash")
	}
	serve(t, m, sid, func(ctx context.Context) {
		_ = fl.Add(ctx, "info", "from tab B")
	})
	close(release)
	<-done

	var got []Message
	serve(t, m, sid, func(ctx context.Context) {
		got, _ = fl.Take(ctx)
	})
	texts := map[string]bool{}
	for _, msg := range got {
		texts[msg.Text] = true
	}
	if len(got) != 2 || !texts["from tab A"] || !texts["from tab B"] {
		t.Fatalf("interleaved adds = %+v, want both tab A and tab B", got)
	}
}

func TestConcurrentAddsAllSurvive(t *testing.T) {
	m, fl := managerWithRetries(t, 100)
	sid := serve(t, m, "", func(ctx context.Context) {
		_ = m.Save(ctx, appSession{Name: "x"})
	})

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			serve(t, m, sid, func(ctx context.Context) {
				errs[i] = fl.Add(ctx, "info", fmt.Sprintf("msg-%d", i))
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent add %d: %v", i, e)
		}
	}

	var got []Message
	serve(t, m, sid, func(ctx context.Context) {
		got, _ = fl.Take(ctx)
	})
	if len(got) != n {
		t.Fatalf("concurrent adds: got %d messages, want %d", len(got), n)
	}
}

func TestConcurrentTakesDeliverOnce(t *testing.T) {
	m, fl := managerWithRetries(t, 100)
	const msgs = 5
	sid := serve(t, m, "", func(ctx context.Context) {
		for i := 0; i < msgs; i++ {
			_ = fl.Add(ctx, "info", fmt.Sprintf("m%d", i))
		}
	})

	const takers = 8
	results := make([][]Message, takers)
	errs := make([]error, takers)
	var wg sync.WaitGroup
	for i := 0; i < takers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			serve(t, m, sid, func(ctx context.Context) {
				results[i], errs[i] = fl.Take(ctx)
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent take %d: %v", i, e)
		}
	}

	total := 0
	seen := map[string]int{}
	for _, r := range results {
		total += len(r)
		for _, msg := range r {
			seen[msg.Text]++
		}
	}
	if total != msgs {
		t.Fatalf("concurrent takes delivered %d total, want %d (each once)", total, msgs)
	}
	for text, c := range seen {
		if c != 1 {
			t.Fatalf("message %q delivered %d times, want 1", text, c)
		}
	}
}
