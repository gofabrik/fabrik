// Package storetest verifies jobs.Store implementations end to end,
// through the manager and worker as well as directly.
//
//	func TestMyStore(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) storetest.Backend {
//			return storetest.Backend{Store: newMyStore(t), DB: db}
//		})
//	}
package storetest

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jobs "github.com/gofabrik/fabrik/jobs"
)

// Backend provides a store and raw database access for conformance tests.
type Backend struct {
	Store jobs.Store
	DB    *sql.DB
	Q     func(string) string
}

func (b Backend) q(s string) string {
	if b.Q == nil {
		return s
	}
	return b.Q(s)
}

type email struct {
	To string `json:"to"`
}

type orderPlaced struct {
	ID int `json:"id"`
}

type task struct {
	ID int `json:"id"`
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newMgr(t *testing.T, b Backend) *jobs.Manager {
	t.Helper()
	m, err := jobs.New(b.Store, jobs.Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultBackoff:    jobs.ExponentialBackoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		SchedulerInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
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

func makeWorkers(t *testing.T, m *jobs.Manager, count int) {
	t.Helper()
	for range count {
		w, err := jobs.NewWorker(m, jobs.WorkerConfig{
			Concurrency:       4,
			PollInterval:      3 * time.Millisecond,
			LeaseDuration:     timeScale * 300 * time.Millisecond,
			HeartbeatInterval: 60 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = w.Start(context.Background()) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeScale*2*time.Second)
			defer cancel()
			// #nosec G104 -- best-effort worker cleanup after the test completes
			w.Stop(ctx) //nolint:errcheck // best-effort worker cleanup after the test completes
		})
	}
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

// IncompressibleKey returns deterministic data that resists compression.
func IncompressibleKey(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, n)
	state := uint64(0x9E3779B97F4A7C15)
	for i := range out {
		state = state*6364136223846793005 + 1442695040888963407
		out[i] = alphabet[state>>58]
	}
	return string(out)
}

func handlers(kinds ...string) map[jobs.HandlerKey]struct{} {
	h := map[jobs.HandlerKey]struct{}{}
	for _, k := range kinds {
		h[jobs.HandlerKey{Kind: k, HandlerID: k}] = struct{}{}
	}
	return h
}

// Run checks a Store; every backend must pass the whole suite.
func Run(t *testing.T, factory func(t *testing.T) Backend) {
	t.Run("EndToEnd", func(t *testing.T) {
		m := newMgr(t, factory(t))
		var got atomic.Value
		requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e email) error { got.Store(e.To); return nil }))
		runWorker(t, m)
		id, err := m.Enqueue(context.Background(), email{To: "a@b.c"})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { return got.Load() == "a@b.c" }, "handler ran")
		eventually(t, func() bool { return state(t, m, id) == jobs.StateSucceeded }, "succeeded")
	})

	t.Run("RetryDiscardLedger", func(t *testing.T) {
		m := newMgr(t, factory(t))
		var runs atomic.Int32
		requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e email) error { runs.Add(1); return errors.New("boom") }))
		runWorker(t, m)
		id, err := m.Enqueue(context.Background(), email{}, jobs.MaxAttempts(2))
		requireNoError(t, err)
		eventually(t, func() bool { return state(t, m, id) == jobs.StateDiscarded }, "discarded")
		if runs.Load() != 2 {
			t.Fatalf("want 2 runs, got %d", runs.Load())
		}
		atts, _ := m.ListJobAttempts(context.Background(), id)
		if len(atts) != 2 {
			t.Fatalf("want 2 attempts, got %d", len(atts))
		}
	})

	t.Run("PublishFanOut", func(t *testing.T) {
		m := newMgr(t, factory(t))
		requireNoError(t, jobs.Register[orderPlaced](m, "order.placed"))
		var a, b atomic.Int32
		requireNoError(t, jobs.On(m, "email", func(_ jobs.Context, o orderPlaced) error { a.Add(1); return nil }))
		requireNoError(t, jobs.On(m, "inventory", func(_ jobs.Context, o orderPlaced) error { b.Add(1); return nil }))
		runWorker(t, m)
		res, err := m.Publish(context.Background(), orderPlaced{ID: 5})
		if err != nil || len(res) != 2 {
			t.Fatalf("publish: res=%d err=%v", len(res), err)
		}
		eventually(t, func() bool { return a.Load() == 1 && b.Load() == 1 }, "both ran")
	})

	t.Run("CancelRunning", func(t *testing.T) {
		m := newMgr(t, factory(t))
		started := make(chan struct{})
		requireNoError(t, jobs.Handle(m, "email", func(ctx jobs.Context, e email) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}))
		runWorker(t, m)
		id, err := m.Enqueue(context.Background(), email{})
		requireNoError(t, err)
		<-started
		if _, err := m.CancelJob(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { return state(t, m, id) == jobs.StateCancelled }, "cancelled")
	})

	t.Run("SweepCancellationWins", func(t *testing.T) {
		b := factory(t)
		m := newMgr(t, b)
		ctx := context.Background()
		now := time.Now().UTC()
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "email", HandlerID: "email", Payload: []byte("{}"),
			Queue: "default", MaxAttempts: 5, AvailableAt: now,
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v", err)
		}
		id := res[0].ID
		claimed, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
			Handlers: handlers("email"),
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v (n=%d)", err, len(claimed))
		}
		if immediate, err := b.Store.Cancel(ctx, id, now); err != nil || immediate {
			t.Fatalf("cancel of a running job: immediate=%v err=%v", immediate, err)
		}
		// Cancellation remains terminal after lease recovery.
		if _, err := b.Store.SweepExpired(ctx, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if got := state(t, m, id); got != jobs.StateCancelled {
			t.Fatalf("state = %q, want %q", got, jobs.StateCancelled)
		}
	})

	t.Run("ClockControlsTimestamps", func(t *testing.T) {
		fixed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		b := factory(t)
		m, err := jobs.New(b.Store, jobs.Config{
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Now:    func() time.Time { return fixed },
		})
		if err != nil {
			t.Fatal(err)
		}
		requireNoError(t, jobs.Handle(m, "email", func(jobs.Context, email) error { return nil }))
		id, err := m.Enqueue(context.Background(), email{})
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
	})

	t.Run("ConcurrentUniqueKey", func(t *testing.T) {
		m := newMgr(t, factory(t))
		requireNoError(t, jobs.Handle(m, "email", func(jobs.Context, email) error { return nil }))
		const n = 16
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = m.Enqueue(context.Background(), email{}, jobs.UniqueKey("k"))
			}(i)
		}
		wg.Wait()
		live := 0
		for _, err := range errs {
			switch {
			case err == nil:
				live++
			case errors.Is(err, jobs.ErrDuplicate):
			default:
				t.Errorf("enqueue: %v", err)
			}
		}
		if live != 1 {
			t.Fatalf("concurrent UniqueKey inserts created %d live jobs, want 1", live)
		}
	})

	t.Run("ConcurrentFireSchedule", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		if err := b.Store.UpsertSchedule(ctx, jobs.ScheduleRow{
			Name: "s", Kind: "email", Spec: "every:1000", Payload: []byte(`{}`), OptionsJSON: []byte(`{}`),
			NextRunAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		job := jobs.Job{Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: now}
		const n = 10
		var wg sync.WaitGroup
		var wins atomic.Int32
		for range n {
			wg.Go(func() {
				won, _, err := b.Store.FireSchedule(ctx, jobs.ScheduleFire{
					Name: "s", ExpectedLastRun: sql.NullTime{Valid: false},
					NewLastRun: now, NewNextRun: now.Add(time.Second), Now: now, Jobs: []jobs.Job{job},
				})
				if err != nil {
					// A loser must lose cleanly, never error.
					t.Error(err)
					return
				}
				if won {
					wins.Add(1)
				}
			})
		}
		wg.Wait()
		// One scheduler wins each tick.
		if wins.Load() != 1 {
			t.Fatalf("concurrent FireSchedule produced %d winners, want exactly 1", wins.Load())
		}
	})

	t.Run("CompleteDoesNotRegressUpdatedAt", func(t *testing.T) {
		b := factory(t)
		m := newMgr(t, b)
		ctx := context.Background()
		early := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		late := early.Add(time.Hour)
		res, err := b.Store.Insert(ctx, early, []jobs.Job{{
			Kind: "email", HandlerID: "email", Payload: []byte("{}"),
			Queue: "default", MaxAttempts: 5, AvailableAt: early,
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v", err)
		}
		id := res[0].ID
		if _, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: early, Lease: time.Minute, Limit: 1,
			Handlers: handlers("email"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Store.Cancel(ctx, id, late); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Store.Complete(ctx, id, "w1", early, jobs.Outcome{
			State: jobs.StateSucceeded, Attempt: 1, AttemptState: jobs.AttemptSucceeded, StartedAt: early, FinishedAt: early,
		}); err != nil {
			t.Fatal(err)
		}
		info, err := m.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !info.UpdatedAt.Equal(late) {
			t.Fatalf("updated_at = %v, want %v (the clamp must not regress under concurrent cancel)", info.UpdatedAt, late)
		}
	})

	t.Run("DedupAndDuplicate", func(t *testing.T) {
		m := newMgr(t, factory(t))
		requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e email) error { return nil }))
		id1, err := m.Enqueue(context.Background(), email{}, jobs.UniqueKey("k"))
		if err != nil {
			t.Fatal(err)
		}
		id2, err := m.Enqueue(context.Background(), email{}, jobs.UniqueKey("k"))
		if !errors.Is(err, jobs.ErrDuplicate) || id2 != id1 {
			t.Fatalf("want dup of %s, got id=%s err=%v", id1, id2, err)
		}
	})

	t.Run("Schedule", func(t *testing.T) {
		m := newMgr(t, factory(t))
		var runs atomic.Int32
		requireNoError(t, jobs.Handle(m, "email", func(_ jobs.Context, e email) error { runs.Add(1); return nil }))
		if err := m.Schedule("poll", jobs.Every(20*time.Millisecond), email{}, jobs.ScheduleOptions{}); err != nil {
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
	})

	t.Run("FireScheduleCAS", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		if err := b.Store.UpsertSchedule(ctx, jobs.ScheduleRow{
			Name: "s", Kind: "email", Spec: "every:1000", Payload: []byte(`{}`), OptionsJSON: []byte(`{}`),
			NextRunAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		job := jobs.Job{Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: now}
		won, res, err := b.Store.FireSchedule(ctx, jobs.ScheduleFire{
			Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Now: now, Jobs: []jobs.Job{job},
		})
		if err != nil || !won || len(res) != 1 {
			t.Fatalf("first fire: won=%v res=%d err=%v", won, len(res), err)
		}
		won, _, err = b.Store.FireSchedule(ctx, jobs.ScheduleFire{
			Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Now: now, Jobs: []jobs.Job{job},
		})
		if err != nil || won {
			t.Fatalf("stale fire should lose: won=%v err=%v", won, err)
		}
	})

	t.Run("FireScheduleMatchedUnchanged", func(t *testing.T) {
		// A matching election wins even when it changes no columns.
		b := factory(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := b.Store.UpsertSchedule(ctx, jobs.ScheduleRow{
			Name: "s", Kind: "email", Spec: "every:1000", Payload: []byte(`{}`), OptionsJSON: []byte(`{}`),
			NextRunAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		fire := jobs.ScheduleFire{
			Name: "s", ExpectedLastRun: sql.NullTime{Valid: false},
			NewLastRun: now, NewNextRun: now, Now: now,
		}
		if won, _, err := b.Store.FireSchedule(ctx, fire); err != nil || !won {
			t.Fatalf("first fire: won=%v err=%v", won, err)
		}
		fire.ExpectedLastRun = sql.NullTime{Time: now, Valid: true}
		won, _, err := b.Store.FireSchedule(ctx, fire)
		if err != nil || !won {
			t.Fatalf("matched-but-unchanged election must win: won=%v err=%v", won, err)
		}
	})

	t.Run("NoDoubleClaim", func(t *testing.T) {
		b := factory(t)
		m, err := jobs.New(b.Store, jobs.Config{
			Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			DefaultBackoff: jobs.ExponentialBackoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		})
		if err != nil {
			t.Fatal(err)
		}
		const n = 100
		var mu sync.Mutex
		seen := map[int]int{}
		requireNoError(t, jobs.Handle(m, "task", func(_ jobs.Context, tk task) error {
			mu.Lock()
			seen[tk.ID]++
			mu.Unlock()
			return nil
		}))
		makeWorkers(t, m, 2)
		for i := range n {
			if _, err := m.Enqueue(context.Background(), task{ID: i}); err != nil {
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
	})

	t.Run("LockedByGuard", func(t *testing.T) {
		b := factory(t)
		if b.DB == nil {
			t.Fatal("backend must provide DB for the locked_by guard check")
		}
		ctx := context.Background()
		now := time.Now().UTC()
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 5, AvailableAt: now,
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v (n=%d, want 1)", err, len(res))
		}
		id := res[0].ID
		claimed, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
			Handlers: handlers("task"),
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v (n=%d)", err, len(claimed))
		}
		// An available row with a lock owner must not be reclaimed.
		if _, err := b.DB.ExecContext(ctx, b.q("UPDATE jobs SET state='available' WHERE id=?"), id); err != nil {
			t.Fatal(err)
		}
		type rowState struct {
			state, lockedBy string
			lockedUntil     int64
			updatedAt       int64
		}
		snap := func() rowState {
			var r rowState
			if err := b.DB.QueryRowContext(ctx,
				b.q("SELECT state, locked_by, locked_until, updated_at FROM jobs WHERE id=?"), id).
				Scan(&r.state, &r.lockedBy, &r.lockedUntil, &r.updatedAt); err != nil {
				t.Fatal(err)
			}
			return r
		}
		before := snap()
		if before.state != "available" {
			t.Fatalf("state = %q after surgical reset, want available", before.state)
		}
		if before.lockedBy == "" {
			t.Fatal("locked_by should still be set after surgical reset")
		}
		claimed2, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w2", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
			Handlers: handlers("task"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed2) != 0 {
			t.Fatalf("locked_by guard failed: got %d claims, want 0", len(claimed2))
		}
		if after := snap(); after != before {
			t.Fatalf("row changed by the skipped claim:\nbefore %+v\nafter  %+v", before, after)
		}
	})

	t.Run("InsertTx", func(t *testing.T) {
		b := factory(t)
		if b.DB == nil {
			t.Fatal("backend must provide DB for the InsertTx check")
		}
		enq, ok := b.Store.(jobs.TxEnqueuer)
		if !ok {
			t.Fatal("store must implement jobs.TxEnqueuer")
		}
		tx, err := b.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		res, err := enq.InsertTx(context.Background(), tx, time.Now().UTC(), []jobs.Job{{
			Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: time.Now(),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Duplicate {
			t.Fatalf("insertTx: %+v", res)
		}
		if _, err := b.Store.Get(context.Background(), res[0].ID); !errors.Is(err, jobs.ErrNotFound) {
			t.Fatalf("pre-commit visible: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Store.Get(context.Background(), res[0].ID); err != nil {
			t.Fatalf("post-commit missing: %v", err)
		}
	})

	t.Run("ClaimOrdering", func(t *testing.T) {
		// Priority desc, then available_at asc, then id asc.
		b := factory(t)
		ctx := context.Background()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		mk := func(prio int, avail time.Time) string {
			res, err := b.Store.Insert(ctx, base, []jobs.Job{{
				Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
				Queue: "default", Priority: prio, MaxAttempts: 1, AvailableAt: avail,
			}})
			if err != nil || len(res) != 1 {
				t.Fatalf("insert: %v", err)
			}
			return res[0].ID
		}
		lowLate := mk(0, base.Add(-time.Second))
		lowEarly := mk(0, base.Add(-2*time.Second))
		high := mk(9, base.Add(-time.Second))
		tieA := mk(0, base.Add(-time.Second))
		tieFirst, tieSecond := lowLate, tieA
		if tieA < lowLate {
			tieFirst, tieSecond = tieA, lowLate
		}
		claimed, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: base, Lease: time.Minute, Limit: 4,
			Handlers: handlers("task"),
		})
		if err != nil || len(claimed) != 4 {
			t.Fatalf("claim: %v (n=%d, want 4)", err, len(claimed))
		}
		got := []string{claimed[0].ID, claimed[1].ID, claimed[2].ID, claimed[3].ID}
		// ID breaks equal-priority, equal-availability ties.
		want := []string{high, lowEarly, tieFirst, tieSecond}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("claim order = %v, want %v (priority desc, available_at asc, id asc)", got, want)
			}
		}
	})

	t.Run("ClaimQueueLimits", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		var js []jobs.Job
		for range 4 {
			js = append(js, jobs.Job{
				Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
				Queue: "a", MaxAttempts: 1, AvailableAt: base.Add(-time.Second),
			})
			js = append(js, jobs.Job{
				Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
				Queue: "b", MaxAttempts: 1, AvailableAt: base.Add(-time.Second),
			})
		}
		if _, err := b.Store.Insert(ctx, base, js); err != nil {
			t.Fatal(err)
		}
		claimed, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"a", "b"}, Now: base, Lease: time.Minute, Limit: 10,
			QueueLimits: map[string]int{"a": 1, "b": 2},
			Handlers:    handlers("task"),
		})
		if err != nil {
			t.Fatal(err)
		}
		per := map[string]int{}
		for _, c := range claimed {
			per[c.Queue]++
		}
		if per["a"] != 1 || per["b"] != 2 || len(claimed) != 3 {
			t.Fatalf("claimed %d (a=%d b=%d), want 3 (a=1 b=2)", len(claimed), per["a"], per["b"])
		}
	})

	t.Run("UniqueKeyAdversarialComponents", func(t *testing.T) {
		// Deduplication uses tuple fields, not a delimiter-joined value.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		ins := func(kind, handler, uk string) jobs.InsertResult {
			res, err := b.Store.Insert(ctx, now, []jobs.Job{{
				Kind: kind, HandlerID: handler, Payload: []byte(`{}`),
				Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk,
			}})
			if err != nil || len(res) != 1 {
				t.Fatalf("insert(%q,%q,%q): %v", kind, handler, uk, err)
			}
			return res[0]
		}
		if r := ins("a|b", "h", "c"); r.Duplicate {
			t.Fatal("first tuple reported duplicate")
		}
		if r := ins("a", "b|h", "c"); r.Duplicate {
			t.Fatal("tuple colliding only after concatenation reported duplicate")
		}
		if r := ins("a", "b", "h|c"); r.Duplicate {
			t.Fatal("tuple colliding only after concatenation reported duplicate")
		}
		if r := ins("e", "h", ""); r.Duplicate {
			t.Fatal("empty unique key reported duplicate")
		}
		if r := ins("e", "h", ""); r.Duplicate {
			t.Fatal("second empty unique key reported duplicate; empty keys never collide")
		}
		if r := ins("", "x", "y"); r.Duplicate {
			t.Fatal("empty kind tuple reported duplicate")
		}
		if r := ins("x", "", "y"); r.Duplicate {
			t.Fatal("tuple sharing only concatenation with the empty-kind row reported duplicate")
		}
		if r := ins("a|b", "h", "c"); !r.Duplicate {
			t.Fatal("exact live tuple repeat must report duplicate")
		}
	})

	t.Run("HeartbeatMatchedUnchanged", func(t *testing.T) {
		// An unchanged extension still confirms lease ownership.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now.Add(-time.Second),
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v", err)
		}
		claimed, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
			Handlers: handlers("task"),
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v (n=%d)", err, len(claimed))
		}
		until := claimed[0].LockedUntil
		for i := range 2 {
			cancelReq, err := b.Store.Heartbeat(ctx, claimed[0].ID, "w1", now, until)
			if err != nil || cancelReq {
				t.Fatalf("identical heartbeat %d: cancel=%v err=%v", i, cancelReq, err)
			}
		}
	})

	t.Run("RepeatedCancelRunning", func(t *testing.T) {
		// Repeated cancellation of a running job remains successful.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now.Add(-time.Second),
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v", err)
		}
		id := res[0].ID
		if _, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
			Handlers: handlers("task"),
		}); err != nil {
			t.Fatal(err)
		}
		if immediate, err := b.Store.Cancel(ctx, id, now); err != nil || immediate {
			t.Fatalf("first cancel of running job: immediate=%v err=%v", immediate, err)
		}
		later := now.Add(time.Minute)
		if immediate, err := b.Store.Cancel(ctx, id, later); err != nil || immediate {
			t.Fatalf("repeated cancel of running job: immediate=%v err=%v", immediate, err)
		}
		info, err := b.Store.Get(ctx, id)
		if err != nil || !info.CancelRequested {
			t.Fatalf("cancel_requested not set: %+v err=%v", info, err)
		}
		if !info.UpdatedAt.Equal(later) {
			t.Fatalf("repeated cancel did not stamp updated_at: %v, want %v", info.UpdatedAt, later)
		}
	})

	t.Run("RetryDuplicateOfLiveHolder", func(t *testing.T) {
		// Retry reports the live key holder instead of a constraint error.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now.Add(-time.Second), UniqueKey: "k",
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v", err)
		}
		terminal := res[0].ID
		if _, err := b.Store.Cancel(ctx, terminal, now); err != nil {
			t.Fatal(err)
		}
		res, err = b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: "k",
		}})
		if err != nil || len(res) != 1 || res[0].Duplicate {
			t.Fatalf("insert holder: %v %+v", err, res)
		}
		var dup *jobs.DuplicateError
		if err := b.Store.Retry(ctx, terminal, now); !errors.As(err, &dup) {
			t.Fatalf("Retry = %v, want *jobs.DuplicateError", err)
		} else if dup.ExistingID != res[0].ID {
			t.Fatalf("DuplicateError.ExistingID = %s, want %s", dup.ExistingID, res[0].ID)
		}
	})

	t.Run("UniqueKeyBoundaryLength", func(t *testing.T) {
		// Verify the portable 2048-byte boundary without compression.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		uk := IncompressibleKey(2048)
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk,
		}})
		if err != nil || len(res) != 1 || res[0].Duplicate {
			t.Fatalf("boundary insert: %v", err)
		}
		res, err = b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk,
		}})
		if err != nil || len(res) != 1 || !res[0].Duplicate {
			t.Fatalf("boundary dedupe: dup=%v err=%v", res[0].Duplicate, err)
		}
	})

	t.Run("ConcurrentHeartbeatUnchanged", func(t *testing.T) {
		// Every identical extension observes the held lease.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now.Add(-time.Second),
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v", err)
		}
		claimed, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
			Handlers: handlers("task"),
		})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v (n=%d)", err, len(claimed))
		}
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				if cancelReq, err := b.Store.Heartbeat(ctx, claimed[0].ID, "w1", now, claimed[0].LockedUntil); err != nil || cancelReq {
					t.Errorf("racing identical heartbeat: cancel=%v err=%v", cancelReq, err)
				}
			})
		}
		wg.Wait()
	})

	t.Run("ConcurrentRepeatedCancel", func(t *testing.T) {
		// Every cancellation of the running job succeeds.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now.Add(-time.Second),
		}})
		if err != nil || len(res) != 1 {
			t.Fatalf("insert: %v", err)
		}
		id := res[0].ID
		if _, err := b.Store.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w1", Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 1,
			Handlers: handlers("task"),
		}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				if immediate, err := b.Store.Cancel(ctx, id, now); err != nil || immediate {
					t.Errorf("racing cancel of running job: immediate=%v err=%v", immediate, err)
				}
			})
		}
		wg.Wait()
	})

	t.Run("ConcurrentFireScheduleUnchanged", func(t *testing.T) {
		// Every serialized unchanged election matches and wins.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := b.Store.UpsertSchedule(ctx, jobs.ScheduleRow{
			Name: "s", Kind: "email", Spec: "every:1000", Payload: []byte(`{}`), OptionsJSON: []byte(`{}`),
			NextRunAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		seed := jobs.ScheduleFire{Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now, Now: now}
		if won, _, err := b.Store.FireSchedule(ctx, seed); err != nil || !won {
			t.Fatalf("seed fire: won=%v err=%v", won, err)
		}
		fire := seed
		fire.ExpectedLastRun = sql.NullTime{Time: now, Valid: true}
		var wg sync.WaitGroup
		var wins atomic.Int32
		for range 10 {
			wg.Go(func() {
				won, _, err := b.Store.FireSchedule(ctx, fire)
				if err != nil {
					t.Error(err)
					return
				}
				if won {
					wins.Add(1)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 10 {
			t.Fatalf("matched-but-unchanged elections won %d of 10, want all", wins.Load())
		}
	})

	t.Run("UniqueKeyInvalidUTF8", func(t *testing.T) {
		// Unique keys accept invalid UTF-8 but exclude NUL.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		uk := "k\xff\xfe"
		res, err := b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk,
		}})
		if err != nil || len(res) != 1 || res[0].Duplicate {
			t.Fatalf("invalid-utf8 unique key insert: %v", err)
		}
		res, err = b.Store.Insert(ctx, now, []jobs.Job{{
			Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
			Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk,
		}})
		if err != nil || len(res) != 1 || !res[0].Duplicate {
			t.Fatalf("invalid-utf8 unique key dedupe: dup=%v err=%v", res[0].Duplicate, err)
		}
		info, err := b.Store.Get(ctx, res[0].ID)
		if err != nil || info.UniqueKey != uk {
			t.Fatalf("invalid-utf8 unique key round-trip: %q err=%v", info.UniqueKey, err)
		}
	})

	t.Run("UniqueKeyByteExact", func(t *testing.T) {
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i, uk := range []string{"Key", "key", "key "} {
			res, err := b.Store.Insert(ctx, now, []jobs.Job{{
				Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
				Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk,
			}})
			if err != nil || len(res) != 1 || res[0].Duplicate {
				t.Fatalf("insert %d (%q): dup=%v err=%v", i, uk, res[0].Duplicate, err)
			}
		}
	})

	t.Run("ConcurrentInsertTxDefaultIsolation", func(t *testing.T) {
		// Exactly one default-isolation transaction wins the unique key.
		b := factory(t)
		if b.DB == nil {
			t.Fatal("backend must provide DB for the InsertTx race")
		}
		enq, ok := b.Store.(jobs.TxEnqueuer)
		if !ok {
			t.Fatal("store must implement jobs.TxEnqueuer")
		}
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		const racers = 6
		var wg sync.WaitGroup
		var wins atomic.Int32
		for range racers {
			wg.Go(func() {
				tx, err := b.DB.Begin()
				if err != nil {
					t.Error(err)
					return
				}
				res, err := enq.InsertTx(ctx, tx, now, []jobs.Job{{
					Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
					Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: "txk",
				}})
				if err != nil {
					// #nosec G104 -- best-effort cleanup after insert error
					tx.Rollback() //nolint:errcheck
					t.Errorf("InsertTx: %v", err)
					return
				}
				if err := tx.Commit(); err != nil {
					t.Errorf("commit: %v", err)
					return
				}
				if len(res) == 1 && !res[0].Duplicate {
					wins.Add(1)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("InsertTx race produced %d winners, want exactly 1", wins.Load())
		}
	})

	t.Run("ConcurrentClaimExactlyOnce", func(t *testing.T) {
		// Concurrent claimers collectively claim every job exactly once.
		b := factory(t)
		ctx := context.Background()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		const jobsN, claimers = 60, 8
		var js []jobs.Job
		for range jobsN {
			js = append(js, jobs.Job{
				Kind: "task", HandlerID: "task", Payload: []byte(`{}`),
				Queue: "default", MaxAttempts: 1, AvailableAt: now.Add(-time.Second),
			})
		}
		if _, err := b.Store.Insert(ctx, now, js); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		counts := map[string]int{}
		var wg sync.WaitGroup
		for w := range claimers {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				worker := "w" + strconv.Itoa(w)
				for {
					claimed, err := b.Store.Claim(ctx, jobs.ClaimRequest{
						WorkerID: worker, Queues: []string{"default"}, Now: now, Lease: time.Minute, Limit: 5,
						Handlers: handlers("task"),
					})
					if err != nil {
						t.Error(err)
						return
					}
					if len(claimed) == 0 {
						return
					}
					mu.Lock()
					for _, c := range claimed {
						counts[c.ID]++
					}
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		if len(counts) != jobsN {
			t.Fatalf("claimed %d distinct jobs, want %d", len(counts), jobsN)
		}
		for id, n := range counts {
			if n != 1 {
				t.Fatalf("job %s claimed %d times", id, n)
			}
		}
	})
}
