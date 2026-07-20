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

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "jobs.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		// #nosec G104 -- best-effort database cleanup after the test completes
		db.Close() //nolint:errcheck // best-effort database cleanup after the test completes
	})
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
		// Allow for race-detector scheduling delays.
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
		// #nosec G104 -- best-effort worker cleanup after the test completes
		w.Stop(ctx) //nolint:errcheck // best-effort worker cleanup after the test completes
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
	requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { got.Store(e.To); return nil }))
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
	requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { runs.Add(1); return errors.New("boom") }))
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
	requireNoError(t, jobs.Register[OrderPlaced](m, "order.placed"))
	var a, b atomic.Int32
	requireNoError(t, jobs.On(m, "email", func(_ jobs.Context, o OrderPlaced) error { a.Add(1); return nil }))
	requireNoError(t, jobs.On(m, "inventory", func(_ jobs.Context, o OrderPlaced) error { b.Add(1); return nil }))
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
	requireNoError(t, jobs.Handle(m, "email", func(ctx jobs.Context, e Email) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}))
	runWorker(t, m)
	id, _ := m.Enqueue(context.Background(), Email{})
	<-started
	if _, err := m.CancelJob(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return state(t, m, id) == jobs.StateCancelled }, "cancelled")
}

func TestSQLiteSweepCancellationWins(t *testing.T) {
	m, store := newMgr(t)
	ctx := context.Background()
	now := time.Now().UTC()
	res, err := store.Insert(ctx, now, []jobs.Job{{
		Kind: "email", HandlerID: "email", Payload: []byte("{}"),
		Queue: "default", MaxAttempts: 5, AvailableAt: now,
	}})
	if err != nil || len(res) != 1 {
		t.Fatalf("insert: %v", err)
	}
	id := res[0].ID
	claimed, err := store.Claim(ctx, jobs.ClaimRequest{
		WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
		Handlers: map[jobs.HandlerKey]struct{}{{Kind: "email", HandlerID: "email"}: {}},
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (n=%d)", err, len(claimed))
	}
	if immediate, err := store.Cancel(ctx, id, now); err != nil || immediate {
		t.Fatalf("cancel of a running job: immediate=%v err=%v", immediate, err)
	}
	// Cancellation remains terminal after lease recovery.
	if _, err := store.SweepExpired(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := state(t, m, id); got != jobs.StateCancelled {
		t.Fatalf("state = %q, want %q", got, jobs.StateCancelled)
	}
}

func TestSQLiteClockControlsTimestamps(t *testing.T) {
	fixed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	store, err := jobs.NewSQLiteStore(openDB(t), jobs.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	m, err := jobs.New(store, jobs.Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNoError(t, jobs.Handle(m, "email", func(jobs.Context, Email) error { return nil }))
	id, err := m.Enqueue(context.Background(), Email{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := m.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Persisted timestamps use Config.Now.
	if !info.CreatedAt.Equal(fixed) || !info.UpdatedAt.Equal(fixed) {
		t.Fatalf("timestamps = created %v / updated %v, want injected %v", info.CreatedAt, info.UpdatedAt, fixed)
	}
}

func TestSQLiteConcurrentUniqueKey(t *testing.T) {
	m, _ := newMgr(t)
	requireNoError(t, jobs.Handle(m, "email", func(jobs.Context, Email) error { return nil }))
	const n = 16
	var wg sync.WaitGroup
	dups := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := m.Enqueue(context.Background(), Email{}, jobs.UniqueKey("k"))
			dups[i] = errors.Is(err, jobs.ErrDuplicate)
		}(i)
	}
	wg.Wait()
	live := 0
	for _, d := range dups {
		if !d {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("concurrent UniqueKey inserts created %d live jobs, want 1", live)
	}
}

func TestSQLiteConcurrentFireSchedule(t *testing.T) {
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
	const n = 10
	var wg sync.WaitGroup
	var wins atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, _, err := store.FireSchedule(ctx, jobs.ScheduleFire{
				Name: "s", ExpectedLastRun: sql.NullTime{Valid: false},
				NewLastRun: now, NewNextRun: now.Add(time.Second), Now: now, Jobs: []jobs.Job{job},
			})
			if err == nil && won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	// One scheduler wins each tick.
	if wins.Load() != 1 {
		t.Fatalf("concurrent FireSchedule produced %d winners, want exactly 1", wins.Load())
	}
}

func TestSQLiteCompleteDoesNotRegressUpdatedAt(t *testing.T) {
	m, store := newMgr(t)
	ctx := context.Background()
	early := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	res, err := store.Insert(ctx, early, []jobs.Job{{
		Kind: "email", HandlerID: "email", Payload: []byte("{}"),
		Queue: "default", MaxAttempts: 5, AvailableAt: early,
	}})
	if err != nil || len(res) != 1 {
		t.Fatalf("insert: %v", err)
	}
	id := res[0].ID
	if _, err := store.Claim(ctx, jobs.ClaimRequest{
		WorkerID: "w1", Queues: []string{"default"}, Now: early, Lease: time.Minute, Limit: 1,
		Handlers: map[jobs.HandlerKey]struct{}{{Kind: "email", HandlerID: "email"}: {}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Cancel(ctx, id, late); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(ctx, id, "w1", early, jobs.Outcome{
		State: jobs.StateSucceeded, Attempt: 1, AttemptState: jobs.AttemptSucceeded, StartedAt: early, FinishedAt: early,
	}); err != nil {
		t.Fatal(err)
	}
	info, err := m.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdatedAt.Equal(late) {
		t.Fatalf("updated_at = %v, want %v (MAX clamp must not regress under concurrent cancel)", info.UpdatedAt, late)
	}
}

func TestSQLiteDedupAndDuplicate(t *testing.T) {
	m, _ := newMgr(t)
	requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { return nil }))
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
	requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { runs.Add(1); return nil }))
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
		Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Now: now, Jobs: []jobs.Job{job},
	})
	if err != nil || !won || len(res) != 1 {
		t.Fatalf("first fire: won=%v res=%d err=%v", won, len(res), err)
	}
	won, _, err = store.FireSchedule(ctx, jobs.ScheduleFire{
		Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Now: now, Jobs: []jobs.Job{job},
	})
	if err != nil || won {
		t.Fatalf("stale fire should lose: won=%v err=%v", won, err)
	}
}

func TestSQLiteNoDoubleClaim(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "race.db") + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G104 -- best-effort database cleanup after the test completes
	defer db.Close() //nolint:errcheck // best-effort database cleanup after the test completes
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
	requireNoError(t, jobs.Handle(m, "task", func(_ jobs.Context, task Task) error {
		mu.Lock()
		seen[task.ID]++
		mu.Unlock()
		return nil
	}))

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
			// #nosec G104 -- best-effort worker cleanup after the test completes
			w.Stop(ctx) //nolint:errcheck // best-effort worker cleanup after the test completes
		})
	}
	return ids
}

func TestSQLiteInsertTx(t *testing.T) {
	m, store := newMgr(t)
	requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e Email) error { return nil }))
	db := openDB(t)
	s2, err := jobs.NewSQLiteStore(db, jobs.SQLiteOptions{AutoCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	res, err := s2.InsertTx(context.Background(), tx, time.Now().UTC(), []jobs.Job{{
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
