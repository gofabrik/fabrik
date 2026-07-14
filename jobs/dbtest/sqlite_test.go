package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jobs "github.com/gofabrik/fabrik/jobs"
	_ "modernc.org/sqlite"
)

type Email struct {
	To string `json:"to"`
}

type OrderPlaced struct {
	ID int `json:"id"`
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "jobs.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newMgr(t *testing.T) (*jobs.Manager, *jobs.SQLiteStore) {
	t.Helper()
	store, err := jobs.NewSQLiteStore(openDB(t), jobs.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	m, err := jobs.New(store, jobs.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultBackoff:    jobs.ExponentialBackoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		SchedulerInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, store
}

func runWorker(t *testing.T, m *jobs.Manager) {
	t.Helper()
	w, err := jobs.NewWorker(m, jobs.WorkerConfig{
		PollInterval: 5 * time.Millisecond,
		// Keep leases long enough for -race scheduler stalls.
		LeaseDuration:     timeScale * 150 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond,
		SweepInterval:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	go func() { _ = w.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeScale*2*time.Second)
		defer cancel()
		w.Stop(ctx)
	})
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeScale * 3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", msg)
}

func state(t *testing.T, m *jobs.Manager, id string) jobs.State {
	t.Helper()
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	return info.State
}

func TestSQLiteEndToEnd(t *testing.T) {
	m, _ := newMgr(t)
	var got atomic.Value
	jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { got.Store(e.To); return nil })
	runWorker(t, m)
	id, err := m.Enqueue(context.Background(), Email{To: "a@b.c"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return got.Load() == "a@b.c" }, "handler ran")
	eventually(t, func() bool { return state(t, m, id) == jobs.StateSucceeded }, "succeeded")
}

func TestSQLiteRetryDiscardLedger(t *testing.T) {
	m, _ := newMgr(t)
	var runs atomic.Int32
	jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { runs.Add(1); return errors.New("boom") })
	runWorker(t, m)
	id, _ := m.Enqueue(context.Background(), Email{}, jobs.MaxAttempts(2))
	eventually(t, func() bool { return state(t, m, id) == jobs.StateDiscarded }, "discarded")
	if runs.Load() != 2 {
		t.Fatalf("want 2 runs, got %d", runs.Load())
	}
	atts, _ := m.ListJobAttempts(context.Background(), id)
	if len(atts) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(atts))
	}
}

func TestSQLitePublishFanOut(t *testing.T) {
	m, _ := newMgr(t)
	jobs.Register[OrderPlaced](m, "order.placed")
	var a, b atomic.Int32
	jobs.On(m, "email", func(_ jobs.Context, o OrderPlaced) error { a.Add(1); return nil })
	jobs.On(m, "inventory", func(_ jobs.Context, o OrderPlaced) error { b.Add(1); return nil })
	runWorker(t, m)
	res, err := m.Publish(context.Background(), OrderPlaced{ID: 5})
	if err != nil || len(res) != 2 {
		t.Fatalf("publish: res=%d err=%v", len(res), err)
	}
	eventually(t, func() bool { return a.Load() == 1 && b.Load() == 1 }, "both ran")
}

func TestSQLiteCancelRunning(t *testing.T) {
	m, _ := newMgr(t)
	started := make(chan struct{})
	jobs.Handle(m, "email", func(ctx jobs.Context, e Email) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	runWorker(t, m)
	id, _ := m.Enqueue(context.Background(), Email{})
	<-started
	if _, err := m.CancelJob(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return state(t, m, id) == jobs.StateCancelled }, "cancelled")
}

func TestSQLiteDedupAndDuplicate(t *testing.T) {
	m, _ := newMgr(t)
	jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { return nil })
	id1, err := m.Enqueue(context.Background(), Email{}, jobs.UniqueKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := m.Enqueue(context.Background(), Email{}, jobs.UniqueKey("k"))
	if !errors.Is(err, jobs.ErrDuplicate) || id2 != id1 {
		t.Fatalf("want dup of %s, got id=%s err=%v", id1, id2, err)
	}
}

func TestSQLiteSchedule(t *testing.T) {
	m, _ := newMgr(t)
	var runs atomic.Int32
	jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { runs.Add(1); return nil })
	if err := m.Schedule("poll", jobs.Every(20*time.Millisecond), Email{}, jobs.ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}
	runWorker(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.ReconcileSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { _ = m.StartScheduler(ctx) }()
	eventually(t, func() bool { return runs.Load() >= 2 }, "schedule fired")
}

func TestSQLiteFireScheduleCAS(t *testing.T) {
	_, store := newMgr(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.UpsertSchedule(ctx, jobs.ScheduleRow{
		Name: "s", Kind: "email", Spec: "every:1000", Payload: []byte(`{}`), OptionsJSON: []byte(`{}`),
		NextRunAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: now}
	won, res, err := store.FireSchedule(ctx, jobs.ScheduleFire{
		Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Jobs: []jobs.Job{job},
	})
	if err != nil || !won || len(res) != 1 {
		t.Fatalf("first fire: won=%v res=%d err=%v", won, len(res), err)
	}
	won, _, err = store.FireSchedule(ctx, jobs.ScheduleFire{
		Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Jobs: []jobs.Job{job},
	})
	if err != nil || won {
		t.Fatalf("stale fire should lose: won=%v err=%v", won, err)
	}
}

// TestSQLiteNoDoubleClaim pins guarded claim behavior with a plain DSN.
func TestSQLiteNoDoubleClaim(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "race.db") + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := jobs.NewSQLiteStore(db, jobs.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	m, err := jobs.New(store, jobs.Config{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultBackoff: jobs.ExponentialBackoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}

	const n = 100
	var mu sync.Mutex
	seen := map[int]int{}
	jobs.Handle(m, "task", func(_ jobs.Context, task Task) error {
		mu.Lock()
		seen[task.ID]++
		mu.Unlock()
		return nil
	})

	for _, id := range makeWorkers(t, m, 2) {
		_ = id
	}
	for i := 0; i < n; i++ {
		if _, err := m.Enqueue(context.Background(), Task{ID: i}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == n
	}, "all jobs ran")

	mu.Lock()
	defer mu.Unlock()
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("job %d ran %d times (double-claim)", id, count)
		}
	}
}

type Task struct {
	ID int `json:"id"`
}

func makeWorkers(t *testing.T, m *jobs.Manager, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		w, err := jobs.NewWorker(m, jobs.WorkerConfig{
			Concurrency:       4,
			PollInterval:      3 * time.Millisecond,
			LeaseDuration:     300 * time.Millisecond,
			HeartbeatInterval: 60 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, w.ID())
		go func() { _ = w.Start(context.Background()) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			w.Stop(ctx)
		})
	}
	return ids
}

func TestSQLiteInsertTx(t *testing.T) {
	m, store := newMgr(t)
	jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { return nil })
	db := openDB(t)
	s2, _ := jobs.NewSQLiteStore(db, jobs.SQLiteOptions{AutoCreate: true})
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	res, err := s2.InsertTx(context.Background(), tx, []jobs.Job{{
		Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Duplicate {
		t.Fatalf("insertTx: %+v", res)
	}
	if _, err := s2.Get(context.Background(), res[0].ID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("pre-commit visible: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(context.Background(), res[0].ID); err != nil {
		t.Fatalf("post-commit missing: %v", err)
	}
	_ = store
}
