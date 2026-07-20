package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerConfig configures a worker. The zero value is valid.
type WorkerConfig struct {
	// ID identifies this worker in locked_by and ListWorkers. Generated
	// when empty.
	ID string
	// Queues this worker pulls from. Defaults to the manager's default.
	Queues []string
	// Concurrency caps in-flight jobs on this worker. Defaults to 1.
	Concurrency int
	// PerQueue caps in-flight jobs per queue. Unset keys are unlimited.
	PerQueue map[string]int
	// PollInterval is the claim cadence. Defaults to 1s.
	PollInterval time.Duration
	// LeaseDuration is how long a claim holds before it can be swept.
	// Defaults to 60s.
	LeaseDuration time.Duration
	// HeartbeatInterval is how often a running job extends its lease and
	// checks for cancellation. Defaults to LeaseDuration/3.
	HeartbeatInterval time.Duration
	// SweepInterval is the housekeeping cadence. Defaults to 30s.
	SweepInterval time.Duration
	// ShutdownTimeout bounds draining after Start's context ends. Zero defaults
	// to 30s; negative cancels immediately. Use Stop(context.Background()) for
	// an unbounded drain.
	ShutdownTimeout time.Duration
}

// Worker claims and executes jobs.
type Worker struct {
	manager  *Manager
	cfg      WorkerConfig
	handlers map[HandlerKey]struct{}

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}

	drainMu  sync.Mutex
	drainCtx context.Context
	drainErr error // drain result, published before doneCh closes

	started   atomic.Bool
	draining  atomic.Bool
	abandoned atomic.Bool // any attempt abandoned at the drain deadline

	sem           chan struct{}
	wg            sync.WaitGroup
	active        sync.Map // jobID -> *runState
	queueInFlight sync.Map // queue -> *atomic.Int32
}

// runPhase serializes handler start and completion against shutdown abandonment.
const (
	phaseRegistered int32 = iota // in active; handler not yet invoked
	phaseRunning                 // handler invoked
	phaseResolving               // runner is committing the outcome
	phaseAbandoned               // drain claimed it (terminal)
)

type runState struct {
	mu     sync.Mutex
	cancel context.CancelFunc // cancels the handler context
	hbStop context.CancelFunc // stops the heartbeat (so the lease is not renewed)
	phase  atomic.Int32       // a runPhase
}

// setCancels publishes cancellation functions before the attempt starts.
func (rs *runState) setCancels(cancel, hbStop context.CancelFunc) {
	rs.mu.Lock()
	rs.cancel, rs.hbStop = cancel, hbStop
	rs.mu.Unlock()
}

// abandon stops any published heartbeat and handler contexts.
func (rs *runState) abandon() {
	rs.mu.Lock()
	cancel, hbStop := rs.cancel, rs.hbStop
	rs.mu.Unlock()
	if hbStop != nil {
		hbStop()
	}
	if cancel != nil {
		cancel()
	}
}

const staleWorkerMultiplier = 5

// NewWorker constructs a worker bound to a manager.
func NewWorker(m *Manager, cfg WorkerConfig) (*Worker, error) {
	if m == nil {
		return nil, fmt.Errorf("jobs.NewWorker: nil manager")
	}
	if cfg.ID == "" {
		cfg.ID = workerID()
	}
	if len(cfg.Queues) == 0 {
		cfg.Queues = []string{m.config.DefaultQueue}
	}
	for _, q := range cfg.Queues {
		if err := validIdent("queue", q); err != nil {
			return nil, err
		}
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 60 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = cfg.LeaseDuration / 3
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	if cfg.LeaseDuration < 3*cfg.HeartbeatInterval {
		return nil, fmt.Errorf("jobs.NewWorker: LeaseDuration (%v) must be at least 3x HeartbeatInterval (%v)",
			cfg.LeaseDuration, cfg.HeartbeatInterval)
	}
	return &Worker{
		manager: m,
		cfg:     cfg,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		sem:     make(chan struct{}, cfg.Concurrency),
	}, nil
}

// ID returns the worker's identity as persisted in locked_by.
func (w *Worker) ID() string { return w.cfg.ID }

// Start runs until ctx ends or [Worker.Stop] is called, then drains. It returns
// context.DeadlineExceeded if the drain deadline expires with unfinished work.
func (w *Worker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return ErrWorkerAlreadyStarted
	}
	defer close(w.doneCh)
	w.handlers = w.manager.handlerSet()

	startedAt := w.manager.now()
	host, _ := os.Hostname()
	regCtx, regCancel := w.manager.withStoreTimeout(ctx)
	if err := w.manager.store.UpsertWorker(regCtx, WorkerRow{
		ID: w.cfg.ID, Hostname: host, Queues: w.cfg.Queues, StartedAt: startedAt, LastSeenAt: startedAt,
	}); err != nil {
		w.manager.config.Logger.Warn("jobs: worker register failed", "worker", w.cfg.ID, "err", err)
	}
	regCancel()
	defer func() {
		retCtx, retCancel := w.manager.withStoreTimeout(context.Background())
		defer retCancel()
		_ = w.manager.store.RetireWorker(retCtx, w.cfg.ID)
	}()

	poll := time.NewTicker(w.cfg.PollInterval)
	defer poll.Stop()
	sweep := time.NewTicker(w.cfg.SweepInterval)
	defer sweep.Stop()

	var drainCtx context.Context
	var drainCancel context.CancelFunc
	defer func() {
		if drainCancel != nil {
			drainCancel()
		}
	}()
loop:
	for {
		select {
		case <-ctx.Done():
			drainCtx, drainCancel = w.shutdownDrainCtx()
			break loop
		case <-w.stopCh:
			w.drainMu.Lock()
			drainCtx = w.drainCtx
			w.drainMu.Unlock()
			if drainCtx == nil {
				drainCtx = context.Background()
			}
			break loop
		case <-poll.C:
			w.tryClaim(ctx)
		case <-sweep.C:
			w.trySweep(ctx)
		}
	}
	err := w.drain(drainCtx)
	// Publish the result before unblocking Stop.
	w.drainMu.Lock()
	w.drainErr = err
	w.drainMu.Unlock()
	return err
}

func (w *Worker) shutdownDrainCtx() (context.Context, context.CancelFunc) {
	if w.cfg.ShutdownTimeout < 0 {
		c, cancel := context.WithCancel(context.Background())
		cancel() // already-expired: cancel in-flight immediately
		return c, func() {}
	}
	// #nosec G118 -- the caller receives and owns the shutdown cancellation function
	return context.WithTimeout(context.Background(), w.cfg.ShutdownTimeout)
}

// Stop signals shutdown and waits for Start. It is idempotent; the first call
// fixes ctx as the drain deadline. Before Start, it returns immediately.
func (w *Worker) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.drainMu.Lock()
		w.drainCtx = ctx
		w.drainMu.Unlock()
		close(w.stopCh)
	})
	if w.started.Load() {
		<-w.doneCh
	}
	w.drainMu.Lock()
	defer w.drainMu.Unlock()
	return w.drainErr
}

func (w *Worker) tryClaim(ctx context.Context) {
	available := cap(w.sem) - len(w.sem)
	if available <= 0 {
		return
	}
	queues, budget := w.eligibleQueues()
	if len(queues) == 0 {
		return
	}
	claimCtx, cancel := w.manager.withStoreTimeout(ctx)
	rows, err := w.manager.store.Claim(claimCtx, ClaimRequest{
		WorkerID:    w.cfg.ID,
		Queues:      queues,
		Now:         w.manager.now(),
		Lease:       w.cfg.LeaseDuration,
		Limit:       available,
		QueueLimits: budget,
		Handlers:    w.handlers,
	})
	cancel()
	if err != nil {
		w.manager.config.Logger.Error("jobs: claim failed", "worker", w.cfg.ID, "err", err)
		return
	}
	for _, row := range rows {
		w.sem <- struct{}{}
		w.bumpQueueInFlight(row.Queue, 1)
		// Make the attempt visible to drain before launching it.
		rs := &runState{}
		w.active.Store(row.ID, rs)
		w.wg.Add(1)
		// #nosec G118 -- claimed jobs use worker-managed cancellation independent of the polling context
		go w.run(row, rs)
	}
}

func (w *Worker) eligibleQueues() (queues []string, budget map[string]int) {
	if len(w.cfg.PerQueue) == 0 {
		return w.cfg.Queues, nil
	}
	budget = make(map[string]int, len(w.cfg.PerQueue))
	queues = make([]string, 0, len(w.cfg.Queues))
	for _, q := range w.cfg.Queues {
		limit, capped := w.cfg.PerQueue[q]
		if !capped || limit <= 0 {
			queues = append(queues, q)
			continue
		}
		remaining := limit - int(w.queueInFlightCount(q))
		if remaining <= 0 {
			continue
		}
		queues = append(queues, q)
		budget[q] = remaining
	}
	return queues, budget
}

func (w *Worker) bumpQueueInFlight(queue string, delta int32) {
	v, _ := w.queueInFlight.LoadOrStore(queue, new(atomic.Int32))
	v.(*atomic.Int32).Add(delta)
}

func (w *Worker) queueInFlightCount(queue string) int32 {
	if v, ok := w.queueInFlight.Load(queue); ok {
		return v.(*atomic.Int32).Load()
	}
	return 0
}

func (w *Worker) trySweep(ctx context.Context) {
	sweepCtx, cancel := w.manager.withStoreTimeout(ctx)
	n, err := w.manager.store.SweepExpired(sweepCtx, w.manager.now())
	cancel()
	if err != nil {
		w.manager.config.Logger.Error("jobs: sweep failed", "worker", w.cfg.ID, "err", err)
	} else if n > 0 {
		w.manager.config.Logger.Info("jobs: reclaimed expired jobs", "worker", w.cfg.ID, "count", n)
	}

	staleBefore := w.manager.now().Add(-staleWorkerMultiplier * w.cfg.LeaseDuration)
	staleCtx, cancel2 := w.manager.withStoreTimeout(ctx)
	removed, err := w.manager.store.SweepStaleWorkers(staleCtx, staleBefore)
	cancel2()
	if err != nil {
		w.manager.config.Logger.Warn("jobs: stale-worker sweep failed", "worker", w.cfg.ID, "err", err)
	} else if removed > 0 {
		w.manager.config.Logger.Info("jobs: removed stale workers", "worker", w.cfg.ID, "count", removed)
	}

	now := w.manager.now()
	host, _ := os.Hostname()
	renewCtx, cancel3 := w.manager.withStoreTimeout(ctx)
	_ = w.manager.store.UpsertWorker(renewCtx, WorkerRow{ID: w.cfg.ID, Hostname: host, Queues: w.cfg.Queues, StartedAt: now, LastSeenAt: now})
	cancel3()
}

// drain abandons active attempts when ctx ends. Resolving attempts may finish
// after it returns.
func (w *Worker) drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		w.draining.Store(true)
		w.active.Range(func(_, v any) bool {
			rs := v.(*runState)
			// Resolving attempts retain ownership of completion.
			if rs.phase.CompareAndSwap(phaseRegistered, phaseAbandoned) ||
				rs.phase.CompareAndSwap(phaseRunning, phaseAbandoned) {
				rs.abandon()
				w.abandoned.Store(true)
			}
			return true
		})
		// Any abandonment or unfinished resolution makes the drain incomplete.
		select {
		case <-done:
			if w.abandoned.Load() {
				return context.DeadlineExceeded
			}
			return nil
		default:
			return context.DeadlineExceeded
		}
	}
}

func workerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), NewID()[:8])
}

// run executes a claimed job whose state is already visible to drain.
func (w *Worker) run(row ClaimedJob, rs *runState) {
	defer w.wg.Done()
	defer func() { <-w.sem }()
	defer w.bumpQueueInFlight(row.Queue, -1)
	defer w.active.Delete(row.ID)

	bg := context.Background()
	attemptNum := row.Attempt + 1
	logger := w.manager.config.Logger.With("job_id", row.ID, "kind", row.Kind, "handler", row.HandlerID)

	runCtx, cancel := context.WithCancel(bg)
	defer cancel()
	if row.TimeoutMs > 0 {
		var tcancel context.CancelFunc
		runCtx, tcancel = context.WithTimeout(runCtx, time.Duration(row.TimeoutMs)*time.Millisecond)
		defer tcancel()
	}
	hbCtx, hbStop := context.WithCancel(bg)
	defer hbStop()

	rs.setCancels(cancel, hbStop)

	entry, ok := w.manager.handlerFor(HandlerKey{Kind: row.Kind, HandlerID: row.HandlerID})
	if !ok {
		w.park(bg, rs, row, attemptNum, logger, fmt.Errorf("%w: %s/%s", ErrUnregisteredHandler, row.Kind, row.HandlerID))
		return
	}
	msg, err := entry.decode(row.Payload)
	if err != nil {
		w.park(bg, rs, row, attemptNum, logger, err)
		return
	}

	// Only the winner of registered -> running may invoke the handler.
	if !rs.phase.CompareAndSwap(phaseRegistered, phaseRunning) {
		logger.Warn("jobs: attempt abandoned before start; handler not invoked", "job_id", row.ID)
		return
	}

	var cancelByUser atomic.Bool
	var auxWg sync.WaitGroup
	auxWg.Add(1)
	go func() {
		defer auxWg.Done()
		w.heartbeat(hbCtx, row.ID, cancel, &cancelByUser)
	}()

	jc := &jobCtx{
		Context: runCtx,
		meta: &jobMeta{
			jobID:        row.ID,
			kind:         row.Kind,
			attempt:      attemptNum,
			logger:       logger,
			scheduledFor: row.ScheduledFor,
			scheduledSet: row.ScheduledForSet,
		},
	}

	start := AttemptStartEvent{JobID: row.ID, Kind: row.Kind, HandlerID: row.HandlerID, Attempt: attemptNum, Logger: logger}
	if w.manager.config.Hooks.OnAttemptStart != nil {
		w.manager.safeHook("OnAttemptStart", func() { w.manager.config.Hooks.OnAttemptStart(runCtx, start) })
	}
	started := w.manager.now()
	runErr := safeRun(jc, entry.invoke, msg)
	finished := w.manager.now()

	// Quiesce the heartbeat before reading cancelByUser.
	hbStop()
	auxWg.Wait()

	// Drain can abandon a running attempt before completion begins.
	if !rs.phase.CompareAndSwap(phaseRunning, phaseResolving) {
		logger.Warn("jobs: attempt abandoned at shutdown deadline; not completing", "job_id", row.ID)
		w.finishHook(runCtx, row, attemptNum, runErr, finished.Sub(started), "", false)
		return
	}

	outcome := w.decideOutcome(row, attemptNum, runErr, cancelByUser.Load(), started, finished)
	applied, committed := w.complete(bg, row, outcome, logger)
	w.finishHook(runCtx, row, attemptNum, runErr, finished.Sub(started), applied, committed)
}

func (w *Worker) finishHook(ctx context.Context, row ClaimedJob, attemptNum int, runErr error, dur time.Duration, state State, committed bool) {
	if w.manager.config.Hooks.OnAttemptFinish == nil {
		return
	}
	w.manager.safeHook("OnAttemptFinish", func() {
		w.manager.config.Hooks.OnAttemptFinish(ctx, AttemptFinishEvent{
			JobID: row.ID, Kind: row.Kind, HandlerID: row.HandlerID, Attempt: attemptNum,
			Err: runErr, Dur: dur, State: state, Committed: committed,
		})
	})
}

// park records a terminal failure unless drain already abandoned the attempt.
func (w *Worker) park(bg context.Context, rs *runState, row ClaimedJob, attemptNum int, logger *slog.Logger, cause error) {
	if !rs.phase.CompareAndSwap(phaseRegistered, phaseResolving) {
		logger.Warn("jobs: attempt abandoned before park; not parking", "job_id", row.ID)
		return
	}
	now := w.manager.now()
	start := AttemptStartEvent{JobID: row.ID, Kind: row.Kind, HandlerID: row.HandlerID, Attempt: attemptNum, Logger: logger}
	if w.manager.config.Hooks.OnAttemptStart != nil {
		w.manager.safeHook("OnAttemptStart", func() { w.manager.config.Hooks.OnAttemptStart(bg, start) })
	}
	outcome := Outcome{
		State: StateFailed, Attempt: attemptNum, Error: cause.Error(),
		AttemptState: AttemptFailed, StartedAt: now, FinishedAt: now,
	}
	applied, committed := w.complete(bg, row, outcome, logger)
	w.finishHook(bg, row, attemptNum, cause, 0, applied, committed)
}

func (w *Worker) decideOutcome(row ClaimedJob, attemptNum int, runErr error, cancelByUser bool, started, finished time.Time) Outcome {
	o := Outcome{Attempt: attemptNum, StartedAt: started, FinishedAt: finished}
	if runErr != nil {
		o.Error = runErr.Error()
	}

	if cancelByUser {
		o.State, o.AttemptState = StateCancelled, AttemptCancelled
		return o
	}
	if runErr == nil {
		o.State, o.AttemptState = StateSucceeded, AttemptSucceeded
		return o
	}
	// Shutdown cancellation releases the job without consuming an attempt.
	if w.draining.Load() && errors.Is(runErr, context.Canceled) {
		o.State = StateAvailable
		o.Attempt = row.Attempt // unchanged
		o.AttemptState = ""     // release, no ledger
		o.AvailableAt = w.manager.now()
		return o
	}
	if errors.Is(runErr, ErrPermanent) {
		o.State, o.AttemptState = StateFailed, AttemptFailed
		return o
	}
	if errors.Is(runErr, context.DeadlineExceeded) && row.TimeoutMs > 0 {
		return w.dispatchTimeout(row, attemptNum, o)
	}
	if attemptNum >= row.MaxAttempts {
		o.State, o.AttemptState = StateDiscarded, AttemptFailed
		return o
	}
	o.State, o.AttemptState = StatePending, AttemptFailed
	o.AvailableAt = w.manager.now().Add(w.backoffFor(row).Next(attemptNum))
	return o
}

func (w *Worker) dispatchTimeout(row ClaimedJob, attemptNum int, o Outcome) Outcome {
	o.AttemptState = AttemptTimedOut
	switch row.OnTimeout {
	case TimeoutFail:
		o.State = StateFailed
	case TimeoutDiscard:
		o.State = StateDiscarded
	default: // TimeoutRetry
		if attemptNum >= row.MaxAttempts {
			o.State = StateDiscarded
		} else {
			o.State = StatePending
			o.AvailableAt = w.manager.now().Add(w.backoffFor(row).Next(attemptNum))
		}
	}
	return o
}

func (w *Worker) backoffFor(row ClaimedJob) Backoff {
	if b := decodeBackoff(row.BackoffSpec); b != nil {
		return b
	}
	return w.manager.config.DefaultBackoff
}

// complete retries transient failures with a fixed budget. ErrNotFound means
// ownership was lost.
func (w *Worker) complete(bg context.Context, row ClaimedJob, o Outcome, logger *slog.Logger) (applied State, committed bool) {
	const maxAttempts = 5
	const maxBackoff = 2 * time.Second
	backoff := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		cctx, cancel := w.manager.withStoreTimeout(bg)
		applied, err := w.manager.store.Complete(cctx, row.ID, w.cfg.ID, w.manager.now(), o)
		cancel()
		if err == nil {
			return applied, true
		}
		if errors.Is(err, ErrNotFound) {
			logger.Warn("jobs: complete abandoned, ownership lost", "err", err)
			return "", false
		}
		if attempt >= maxAttempts {
			logger.Warn("jobs: complete failed after retries, giving up", "attempts", attempt, "err", err)
			return "", false
		}
		time.Sleep(backoff)
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (w *Worker) heartbeat(ctx context.Context, jobID string, cancel context.CancelFunc, cancelByUser *atomic.Bool) {
	ticker := time.NewTicker(w.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, hbCancel := w.manager.withStoreTimeout(ctx)
			now := w.manager.now()
			cancelRequested, err := w.manager.store.Heartbeat(hbCtx, jobID, w.cfg.ID, now, now.Add(w.cfg.LeaseDuration))
			hbCancel()
			if err != nil {
				if ctx.Err() != nil {
					return // clean-shutdown race
				}
				if errors.Is(err, ErrNotFound) {
					cancel()
					return
				}
				w.manager.config.Logger.Warn("jobs: heartbeat failed", "worker", w.cfg.ID, "job_id", jobID, "err", err)
				continue
			}
			if cancelRequested {
				cancelByUser.Store(true)
				cancel()
				return
			}
		}
	}
}

// safeRun converts handler panics to errors.
func safeRun(ctx Context, invoke func(Context, any) error, msg any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return invoke(ctx, msg)
}
