package jobs

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func schedManager(t *testing.T, store Store) *Manager {
	t.Helper()
	m, err := New(store, Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		DefaultBackoff:    ExponentialBackoff{Base: time.Millisecond, Max: 5 * time.Millisecond},
		SchedulerInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestPlanFires(t *testing.T) {
	loc := time.UTC
	now := time.Now().UTC()
	spec := Every(10 * time.Minute)

	nextRun := now.Add(-25 * time.Minute)
	ticks, next, capped, err := planFires(spec, loc, nextRun, now, CatchUpAll)
	if err != nil {
		t.Fatal(err)
	}
	if capped || len(ticks) != 3 {
		t.Fatalf("CatchUpAll: want 3 ticks, got %d (capped=%v)", len(ticks), capped)
	}
	if !next.After(now) {
		t.Fatalf("next %v should be after now", next)
	}

	ticks, _, _, _ = planFires(spec, loc, nextRun, now, CatchUpOnce)
	if len(ticks) != 1 || !ticks[0].Equal(nextRun) {
		t.Fatalf("CatchUpOnce: want [nextRun], got %v", ticks)
	}

	ticks, next, _, _ = planFires(spec, loc, nextRun, now, CatchUpSkip)
	if len(ticks) != 0 || !next.After(now) {
		t.Fatalf("CatchUpSkip: want 0 ticks and future next, got %d / %v", len(ticks), next)
	}

	future := now.Add(10 * time.Minute)
	ticks, next, _, _ = planFires(spec, loc, future, now, CatchUpOnce)
	if len(ticks) != 0 || !next.Equal(future) {
		t.Fatalf("not-due: want 0 ticks, next preserved")
	}
}

func TestScheduleEveryEndToEnd(t *testing.T) {
	m := schedManager(t, NewMemoryStore())
	var runs atomic.Int32
	Handle[Email](m, "email", func(Context, Email) error { runs.Add(1); return nil })
	if err := m.Schedule("poll", Every(20*time.Millisecond), Email{To: "x"}, ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}
	runWorker(t, m, WorkerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.ReconcileSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { _ = m.StartScheduler(ctx) }()
	eventually(t, func() bool { return runs.Load() >= 2 }, "schedule fired repeatedly")
}

func TestScheduleRequiresHandler(t *testing.T) {
	m := schedManager(t, NewMemoryStore())
	Register[Email](m, "email")
	if err := m.Schedule("poll", Every(time.Second), Email{}, ScheduleOptions{}); err == nil {
		t.Fatal("want error scheduling a kind with no handler")
	}
}

func TestScheduleDuplicateName(t *testing.T) {
	m := schedManager(t, NewMemoryStore())
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	if err := m.Schedule("poll", Every(time.Minute), Email{}, ScheduleOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Schedule("poll", Every(time.Hour), Email{}, ScheduleOptions{}); !errors.Is(err, ErrScheduleAlreadyDeclared) {
		t.Fatalf("duplicate Schedule name: got %v, want ErrScheduleAlreadyDeclared", err)
	}
}

func TestCronAndScheduleShareNameConflict(t *testing.T) {
	m := schedManager(t, NewMemoryStore())
	noop := func(Context) error { return nil }
	if err := RegisterCron(m, "purge", "0 3 * * *", noop); err != nil {
		t.Fatal(err)
	}
	if err := RegisterCron(m, "purge", "0 4 * * *", noop); !errors.Is(err, ErrScheduleAlreadyDeclared) {
		t.Fatalf("duplicate cron name: got %v, want ErrScheduleAlreadyDeclared", err)
	}
	Handle[Email](m, "email", func(Context, Email) error { return nil })
	if err := m.Schedule("purge", Every(time.Minute), Email{}, ScheduleOptions{}); !errors.Is(err, ErrScheduleAlreadyDeclared) {
		t.Fatalf("schedule reusing a cron name: got %v, want ErrScheduleAlreadyDeclared", err)
	}
}

func TestReconcileDeletesOrphan(t *testing.T) {
	store := NewMemoryStore()
	old := schedManager(t, store)
	Handle[Email](old, "email", func(Context, Email) error { return nil })
	old.Schedule("a", Every(time.Minute), Email{}, ScheduleOptions{})
	old.Schedule("b", Every(time.Minute), Email{}, ScheduleOptions{})
	if err := old.ReconcileSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}

	fresh := schedManager(t, store)
	Handle[Email](fresh, "email", func(Context, Email) error { return nil })
	fresh.Schedule("a", Every(time.Minute), Email{}, ScheduleOptions{})
	if err := fresh.ReconcileSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(context.Background(), "")
	if len(rows) != 1 || rows[0].Name != "a" {
		t.Fatalf("want only [a] after reconcile, got %+v", rows)
	}
}

func TestFireScheduleCASNullFirst(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	store.UpsertSchedule(context.Background(), ScheduleRow{
		Name: "s", Kind: "email", Spec: "every:1000", NextRunAt: now, UpdatedAt: now,
	})
	job := Job{Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: now}

	won, res, err := store.FireSchedule(context.Background(), ScheduleFire{
		Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Jobs: []Job{job},
	})
	if err != nil || !won || len(res) != 1 {
		t.Fatalf("first fire: won=%v res=%d err=%v", won, len(res), err)
	}
	won, res, err = store.FireSchedule(context.Background(), ScheduleFire{
		Name: "s", ExpectedLastRun: sql.NullTime{Valid: false}, NewLastRun: now, NewNextRun: now.Add(time.Second), Jobs: []Job{job},
	})
	if err != nil || won || len(res) != 0 {
		t.Fatalf("stale fire should lose: won=%v res=%d err=%v", won, len(res), err)
	}
}

func TestRegisterCron(t *testing.T) {
	store := NewMemoryStore()
	m := schedManager(t, store)
	var ran atomic.Bool
	if err := RegisterCron(m, "purge", "0 3 * * *", func(c Context) error {
		ran.Store(true)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.ListSchedules(context.Background(), ""); len(rows) != 0 {
		t.Fatalf("registration should not persist; got %+v", rows)
	}
	if err := m.ReconcileSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.ListSchedules(context.Background(), "")
	if len(rows) != 1 || rows[0].Name != "purge" || rows[0].Kind != "cron:purge" {
		t.Fatalf("schedule: %+v", rows)
	}
	runWorker(t, m, WorkerConfig{})
	store.Insert(context.Background(), []Job{{
		Kind: "cron:purge", HandlerID: "purge", Payload: []byte("{}"),
		Queue: "default", MaxAttempts: 1, AvailableAt: time.Now(),
	}})
	eventually(t, func() bool { return ran.Load() }, "cron function ran")
}

func TestSingletonCollapsesCatchUp(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	uk := "schedule:s"
	jobs := []Job{
		{Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk},
		{Kind: "email", HandlerID: "email", Payload: []byte(`{}`), Queue: "default", MaxAttempts: 1, AvailableAt: now, UniqueKey: uk},
	}
	res, _ := store.Insert(context.Background(), jobs)
	if res[0].Duplicate || !res[1].Duplicate {
		t.Fatalf("singleton: first new, second dup, got %+v", res)
	}
}
