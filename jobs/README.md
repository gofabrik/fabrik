# jobs

A durable background-job engine with typed messages, retries, scheduling,
crash recovery, and memory, SQLite, PostgreSQL, or MySQL/MariaDB storage.

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
| `EnqueueTx` / `PublishTx` | Same, inside a `*sql.Tx` (any SQL-backed store). |
| `Schedule(name, spec, msg, opts)` | Declare a recurring message schedule: `jobs.Cron("0 6 * * *")` or `jobs.Every(d)`. Persisted at `ReconcileSchedules`. |

Options ride along as functional options: `After`, `At`, `Queue`,
`Priority`, `MaxAttempts`, `WithBackoff`, `Timeout`, `TimeoutAction`,
`UniqueKey`.

## Reliability

At-least-once. A worker can crash mid-run and the job runs again, so wrap
non-idempotent side effects behind your own idempotency key.

- **Retries** with exponential backoff and jitter; `ErrPermanent`
  short-circuits to failed. `MaxAttempts` counts total executions, including
  the first run; zero selects the manager default of 25.
- **Per-attempt timeout** with a `TimeoutRetry`, `TimeoutFail`, or
  `TimeoutDiscard` policy. Timeout and cancellation are cooperative: the
  deadline cancels the context but cannot terminate the goroutine. Timeout
  policy applies after the handler returns.
- **Cancellation**: `CancelJob` stops a pending job now; a running one is
  requested to stop on its next heartbeat by cancelling its context.
- **Crash recovery**: an expired lease is reclaimed and re-run (or
  discarded at the cap).
- **Concurrency**: `Concurrency` caps in-flight jobs; `PerQueue` caps them
  per queue. With `PerQueue` set, SQL stores apply budgets over a fixed 500-row
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

## Running

```go
drain, err := jobs.Run(ctx, mgr, jobs.RuntimeConfig{
	Worker:       jobs.WorkerConfig{Concurrency: 8},
	RunScheduler: true,
})
if err != nil {
	return err
}
<-ctx.Done() // canceled by a shutdown signal
err = drain(drainCtx)
```

`Run` starts the worker bound to `ctx` and, when `RunScheduler` is set, reconciles
schedules and starts the scheduler; it returns a `drain` to call once at shutdown that,
after `ctx` is cancelled, waits for them to stop within the deadline and returns the first
non-cancellation error. `Run` **always hosts a worker**, so a producer that only enqueues
must not call it. In a worker that should not own scheduling, leave `RunScheduler` off:
reconciliation prunes schedules the process does not declare, so hosting the scheduler is a
deliberate choice. `Run` composes `NewWorker` + `Start` + `StartScheduler`; use those
directly when you need finer control.

## Storage

```go
store := jobs.NewMemoryStore()                       // tests, local dev

store, _ := sqlite.New(db, sqlite.Options{AutoCreate: true})     // jobs/sqlite
store, _ := postgres.New(db, postgres.Options{AutoCreate: true}) // jobs/postgres
store, _ := mysql.New(db, mysql.Options{AutoCreate: true})       // jobs/mysql
```

Each database-backed store lives in its own package under `jobs/` and takes
a caller-opened `*sql.DB`. Construct with `AutoCreate: true`, or apply the
package's `Schema()` through your migrations.

The SQLite store supports one SQLite file used by one or more processes on
a single node. Open it with WAL, a busy timeout, and immediate-locked
transactions; for `modernc.org/sqlite`:

```
file:jobs.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate
```

The `mysql` package serves both MySQL and MariaDB. Its identifier columns
(kind, handler id, queue, worker id, schedule group and name) are
`VARBINARY(255)`, matching the manager's 255-byte identifier bound; job
and attempt ids are store-generated UUIDs in `VARBINARY(191)`;
`unique_key` is `VARBINARY(2048)`. Longer values are rejected, and applying
the multi-statement schema needs `multiStatements=true` in the DSN.
PostgreSQL indexed values are subject to its content-dependent B-tree
limit of roughly 2700 bytes. SQLite has no identifier limit.
