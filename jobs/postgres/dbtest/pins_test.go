// Closed handles fail before dialing, so these tests require no server.
package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/jobs"
	jobspg "github.com/gofabrik/fabrik/jobs/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openClosedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://closed:closed@localhost:1/closed")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// openClosedStore captures the closed handle's error for identity checks.
func openClosedStore(t *testing.T) (*jobspg.Store, error) {
	t.Helper()
	db := openClosedDB(t)
	s, err := jobspg.New(db, jobspg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	ref := db.PingContext(context.Background())
	if ref == nil {
		t.Fatal("closed handle did not error")
	}
	return s, ref
}

// rawPassthrough requires the original error rather than a wrapped equivalent.
func rawPassthrough(t *testing.T, err, ref error) {
	t.Helper()
	if err != ref {
		t.Fatalf("want the raw closed-database error by identity, got: %v", err)
	}
}

func wantPrefix(t *testing.T, err error, prefix string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("want prefix %q, got %v", prefix, err)
	}
}

func TestClosedDB_NilDB(t *testing.T) {
	_, err := jobspg.New(nil, jobspg.Options{})
	if err == nil || err.Error() != "jobs: nil db" {
		t.Fatalf("want \"jobs: nil db\", got %v", err)
	}
}

func TestClosedDB_ErrorPassthrough(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("createSchema", func(t *testing.T) {
		db := openClosedDB(t)
		db.Close()
		_, err := jobspg.New(db, jobspg.Options{AutoCreate: true})
		wantPrefix(t, err, "jobs: create schema: ")
	})

	t.Run("insert", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.Insert(ctx, now, nil)
		rawPassthrough(t, err, ref)
	})

	t.Run("insertTx", func(t *testing.T) {
		// A finished transaction returns sql.ErrTxDone before any driver call.
		tx := finishedTx(t)
		s, _ := openClosedStore(t)
		_, err := s.InsertTx(ctx, tx, now, []jobs.Job{{
			Kind: "k", HandlerID: "h", Queue: "q", MaxAttempts: 1,
		}})
		if err != sql.ErrTxDone {
			t.Fatalf("want sql.ErrTxDone by identity, got %v", err)
		}
	})

	t.Run("claim", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.Claim(ctx, jobs.ClaimRequest{
			WorkerID: "w",
			Queues:   []string{"q"},
			Handlers: map[jobs.HandlerKey]struct{}{{Kind: "k", HandlerID: "h"}: {}},
			Now:      now,
		})
		rawPassthrough(t, err, ref)
	})

	t.Run("heartbeat", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.Heartbeat(ctx, "id", "w", now, now.Add(time.Minute))
		rawPassthrough(t, err, ref)
	})

	t.Run("complete", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.Complete(ctx, "id", "w", now, jobs.Outcome{State: jobs.StateSucceeded})
		rawPassthrough(t, err, ref)
	})

	t.Run("sweepExpired", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.SweepExpired(ctx, now)
		rawPassthrough(t, err, ref)
	})

	t.Run("get", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.Get(ctx, "id")
		rawPassthrough(t, err, ref)
	})

	t.Run("list", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, _, err := s.List(ctx, jobs.ListFilter{})
		rawPassthrough(t, err, ref)
	})

	t.Run("listAttempts", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.ListAttempts(ctx, "id", 0, 10)
		rawPassthrough(t, err, ref)
	})

	t.Run("retry", func(t *testing.T) {
		s, ref := openClosedStore(t)
		err := s.Retry(ctx, "id", now)
		rawPassthrough(t, err, ref)
	})

	t.Run("cancel", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.Cancel(ctx, "id", now)
		rawPassthrough(t, err, ref)
	})

	t.Run("delete", func(t *testing.T) {
		s, ref := openClosedStore(t)
		err := s.Delete(ctx, "id")
		rawPassthrough(t, err, ref)
	})

	t.Run("upsertWorker", func(t *testing.T) {
		s, ref := openClosedStore(t)
		err := s.UpsertWorker(ctx, jobs.WorkerRow{ID: "w"})
		rawPassthrough(t, err, ref)
	})

	t.Run("retireWorker", func(t *testing.T) {
		s, ref := openClosedStore(t)
		err := s.RetireWorker(ctx, "w")
		rawPassthrough(t, err, ref)
	})

	t.Run("listWorkers", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.ListWorkers(ctx)
		rawPassthrough(t, err, ref)
	})

	t.Run("sweepStaleWorkers", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.SweepStaleWorkers(ctx, now)
		rawPassthrough(t, err, ref)
	})

	t.Run("listQueues", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.ListQueues(ctx)
		rawPassthrough(t, err, ref)
	})

	t.Run("upsertSchedule", func(t *testing.T) {
		s, ref := openClosedStore(t)
		err := s.UpsertSchedule(ctx, jobs.ScheduleRow{Group: "g", Name: "n"})
		rawPassthrough(t, err, ref)
	})

	t.Run("deleteSchedule", func(t *testing.T) {
		s, ref := openClosedStore(t)
		err := s.DeleteSchedule(ctx, "g", "n")
		rawPassthrough(t, err, ref)
	})

	t.Run("listSchedules", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.ListSchedules(ctx, "g")
		rawPassthrough(t, err, ref)
	})

	t.Run("dueSchedules", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, err := s.DueSchedules(ctx, "g", now)
		rawPassthrough(t, err, ref)
	})

	t.Run("fireSchedule", func(t *testing.T) {
		s, ref := openClosedStore(t)
		_, _, err := s.FireSchedule(ctx, jobs.ScheduleFire{Group: "g", Name: "n", Now: now})
		rawPassthrough(t, err, ref)
	})
}
