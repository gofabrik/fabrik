# jobs

A durable background-job engine with typed messages, retries, scheduling,
crash recovery, and memory or SQLite storage.

```go
type WelcomeEmail struct{ UserID int64 }

mgr, _ := jobs.New(jobs.NewMemoryStore(), jobs.Config{})

jobs.Handle(mgr, "email.welcome", func(ctx jobs.Context, m WelcomeEmail) error {
	return nil
})

mgr.Enqueue(ctx, WelcomeEmail{UserID: 42})
mgr.Enqueue(ctx, WelcomeEmail{UserID: 7}, jobs.After(24*time.Hour))

w, _ := jobs.NewWorker(mgr, jobs.WorkerConfig{Concurrency: 8})
w.Start(ctx)
```

## Commands and events

A **message** is a plain JSON struct; a **handler** is a function
registered against it. One handler is a **command** (`Enqueue`, exactly
one handler); many handlers on one type are an **event** (`Publish`, fan
out to all). Both become the same durable job, one per handler, retrying
independently.

```go
jobs.Register[OrderPlaced](mgr, "order.placed")
jobs.On(mgr, "email-receipt", emailReceipt)
jobs.On(mgr, "update-inventory", updateStock)
mgr.Publish(ctx, OrderPlaced{ID: 100})
```

| Call | Contract |
|------|----------|
| `Enqueue(ctx, msg, opts...)` | Command: the one handler. Errors on 0 or >1. Returns the job id. |
| `Publish(ctx, msg, opts...)` | Event: one job per handler. Returns a per-handler result. |
| `EnqueueTx` / `PublishTx` | Same, inside a `*sql.Tx` (SQLite store only). |
| `Schedule(name, spec, msg, opts)` | Declare a recurring message schedule: `jobs.Cron("0 6 * * *")` or `jobs.Every(d)`. Persisted at `ReconcileSchedules`. |

Options ride along as functional options: `After`, `At`, `Queue`,
`Priority`, `MaxAttempts`, `WithBackoff`, `Timeout`, `TimeoutAction`,
`UniqueKey`.

## Reliability

At-least-once. A worker can crash mid-run and the job runs again, so wrap
non-idempotent side effects behind your own idempotency key.

- **Retries** with exponential backoff and jitter; `ErrPermanent`
  short-circuits to failed; a bounded `MaxAttempts` (default 25).
- **Per-attempt timeout** with a `TimeoutRetry`, `TimeoutFail`, or
  `TimeoutDiscard` policy. Timeout and cancellation are cooperative: the
  deadline cancels the context but cannot terminate the goroutine. Timeout
  policy applies after the handler returns.
- **Cancellation**: `CancelJob` stops a pending job now; a running one is
  requested to stop on its next heartbeat by cancelling its context.
- **Crash recovery**: an expired lease is reclaimed and re-run (or
  discarded at the cap).
- **Concurrency**: `Concurrency` caps in-flight jobs; `PerQueue` caps them
  per queue. With `PerQueue` set, SQLite applies budgets over a fixed 500-row
  candidate window, so a saturated queue can delay eligible rows beyond it.
- **Inspection**: `GetJob`, `ListJobs`, `ListJobAttempts`, `ListQueues`,
  `ListWorkers`. **Hooks**: `OnEnqueue`, `OnAttemptStart`,
  `OnAttemptFinish`.

## Scheduling

```go
jobs.RegisterCron(mgr, "purge", "0 6 * * *", purge) // declares, in memory

if err := mgr.ReconcileSchedules(ctx); err != nil {
	return err
}
go mgr.StartScheduler(ctx)
```

Code is the source of truth. `RegisterCron` and `Schedule` declare in
memory. `ReconcileSchedules` syncs declarations to the store, pruning
orphaned rows in `Config.SchedulerGroup`; run it once at startup after
the schema exists. `StartScheduler` only fires due schedules. Runtime
one-off work uses `Enqueue` with `At`/`After`, or a job that enqueues its
next run. Multiple processes are safe: a CAS picks one winner per tick,
and each fire advances the schedule and inserts its jobs in one
transaction.

## Storage

```go
store := jobs.NewMemoryStore()                       // tests, local dev

store, _ := jobs.NewSQLiteStore(db, jobs.SQLiteOptions{AutoCreate: true})
```

The SQLite store takes a caller-opened `*sql.DB` and imports no driver.
Open it with WAL, a busy timeout, and immediate-locked transactions; for
`modernc.org/sqlite`:

```
file:jobs.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate
```

Construct with `AutoCreate: true`, or apply `jobs.SQLiteSchema()` through
your migrations. It supports one SQLite file used by one or more processes
on a single node.
