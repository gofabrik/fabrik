package jobs

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
)

// MemoryStore is a non-durable in-process [Store] for tests and development.
type MemoryStore struct {
	mu        sync.Mutex
	rows      map[string]*memRow
	uniq      map[string]string // kind\x00handler\x00key -> live jobID
	attempts  map[string][]Attempt
	workers   map[string]WorkerRow
	schedules map[string]ScheduleRow // group\x00name -> row
}

type memRow struct {
	Job
	id              string
	state           State
	attempt         int
	lockedBy        string
	lockedUntil     time.Time
	cancelRequested bool
	lastError       string
	createdAt       time.Time
	updatedAt       time.Time
}

// NewMemoryStore constructs an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		rows:      make(map[string]*memRow),
		uniq:      make(map[string]string),
		attempts:  make(map[string][]Attempt),
		workers:   make(map[string]WorkerRow),
		schedules: make(map[string]ScheduleRow),
	}
}

func uniqKey(kind, handler, key string) string { return kind + "\x00" + handler + "\x00" + key }
func schedKey(group, name string) string       { return group + "\x00" + name }

func (s *MemoryStore) Insert(_ context.Context, now time.Time, jobs []Job) ([]InsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertLocked(now.UTC(), jobs), nil
}

// insertLocked deduplicates against live jobs and earlier rows in the batch.
func (s *MemoryStore) insertLocked(now time.Time, jobs []Job) []InsertResult {
	out := make([]InsertResult, 0, len(jobs))
	for _, j := range jobs {
		if j.UniqueKey != "" {
			k := uniqKey(j.Kind, j.HandlerID, j.UniqueKey)
			if existingID, ok := s.uniq[k]; ok {
				if r, ok := s.rows[existingID]; ok && !r.state.Terminal() {
					out = append(out, InsertResult{ID: existingID, CreatedAt: r.createdAt, Kind: j.Kind, HandlerID: j.HandlerID, Duplicate: true})
					continue
				}
			}
		}
		id := NewID()
		state := StateAvailable
		if j.AvailableAt.After(now) {
			state = StatePending
		}
		r := &memRow{Job: j, id: id, state: state, createdAt: now, updatedAt: now}
		s.rows[id] = r
		if j.UniqueKey != "" {
			s.uniq[uniqKey(j.Kind, j.HandlerID, j.UniqueKey)] = id
		}
		out = append(out, InsertResult{ID: id, CreatedAt: now, Kind: j.Kind, HandlerID: j.HandlerID, ScheduledFor: j.ScheduledFor})
	}
	return out
}

func (s *MemoryStore) Claim(_ context.Context, req ClaimRequest) ([]ClaimedJob, error) {
	if req.WorkerID == "" || len(req.Handlers) == 0 {
		return nil, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}
	queues := make(map[string]struct{}, len(req.Queues))
	for _, q := range req.Queues {
		queues[q] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var cands []*memRow
	for _, r := range s.rows {
		if r.state != StateAvailable && r.state != StatePending {
			continue
		}
		if !r.AvailableAt.IsZero() && r.AvailableAt.After(req.Now) {
			continue
		}
		if len(queues) > 0 {
			if _, ok := queues[r.Queue]; !ok {
				continue
			}
		}
		if _, ok := req.Handlers[HandlerKey{Kind: r.Kind, HandlerID: r.HandlerID}]; !ok {
			continue
		}
		cands = append(cands, r)
	}
	slices.SortFunc(cands, func(a, b *memRow) int {
		if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
			return c
		}
		if c := a.AvailableAt.Compare(b.AvailableAt); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})

	until := req.Now.Add(req.Lease)
	perQueue := map[string]int{}
	out := make([]ClaimedJob, 0, limit)
	for _, r := range cands {
		if len(out) >= limit {
			break
		}
		if qlim, ok := req.QueueLimits[r.Queue]; ok && qlim >= 0 && perQueue[r.Queue] >= qlim {
			continue
		}
		r.state = StateRunning
		r.lockedBy = req.WorkerID
		r.lockedUntil = until
		r.updatedAt = req.Now
		perQueue[r.Queue]++
		out = append(out, ClaimedJob{Job: r.Job, ID: r.id, Attempt: r.attempt, LockedUntil: until})
	}
	return out, nil
}

func (s *MemoryStore) Heartbeat(_ context.Context, jobID, workerID string, now, until time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok || r.state != StateRunning || r.lockedBy != workerID {
		return false, ErrNotFound
	}
	r.lockedUntil = until
	r.updatedAt = now.UTC()
	return r.cancelRequested, nil
}

func (s *MemoryStore) Complete(_ context.Context, jobID, workerID string, now time.Time, o Outcome) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok || r.state != StateRunning || r.lockedBy != workerID {
		return "", ErrNotFound
	}

	applied := o.State
	attemptState := o.AttemptState
	record := o.AttemptState != ""
	attemptNum := o.Attempt
	if r.cancelRequested {
		applied = StateCancelled
		attemptState = AttemptCancelled
		if !record {
			record = true
			attemptNum = r.attempt + 1
		}
	}

	if record {
		s.attempts[jobID] = append(s.attempts[jobID], Attempt{
			ID: NewID(), JobID: jobID, Attempt: attemptNum, WorkerID: workerID,
			State: attemptState, Error: o.Error, StartedAt: o.StartedAt, FinishedAt: o.FinishedAt,
		})
		r.attempt = attemptNum
	}
	r.state = applied
	r.lastError = o.Error
	if applied == StatePending || applied == StateAvailable {
		r.AvailableAt = o.AvailableAt
	}
	r.lockedBy = ""
	r.lockedUntil = time.Time{}
	r.cancelRequested = false
	// Preserve newer timestamps written by concurrent operations.
	if u := now.UTC(); u.After(r.updatedAt) {
		r.updatedAt = u
	}
	if applied.Terminal() {
		s.freeUniq(r)
	}
	return applied, nil
}

func (s *MemoryStore) SweepExpired(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, r := range s.rows {
		if r.state != StateRunning || r.lockedUntil.IsZero() || r.lockedUntil.After(now) {
			continue
		}
		attemptNum := r.attempt + 1
		attemptState, attemptErr := AttemptFailed, "lease expired"
		if r.cancelRequested {
			attemptState, attemptErr = AttemptCancelled, "cancelled after lease expiry"
		}
		s.attempts[r.id] = append(s.attempts[r.id], Attempt{
			ID: NewID(), JobID: r.id, Attempt: attemptNum, WorkerID: r.lockedBy,
			State: attemptState, Error: attemptErr, StartedAt: r.lockedUntil, FinishedAt: now,
		})
		r.attempt = attemptNum
		switch {
		case r.cancelRequested:
			// Cancellation remains terminal across lease recovery.
			r.state = StateCancelled
			r.lastError = "cancelled"
			s.freeUniq(r)
		case attemptNum >= r.MaxAttempts:
			r.state = StateDiscarded
			r.lastError = "lease expired"
			s.freeUniq(r)
		default:
			r.state = StateAvailable
			r.AvailableAt = now
		}
		r.lockedBy = ""
		r.lockedUntil = time.Time{}
		r.cancelRequested = false
		r.updatedAt = now
		count++
	}
	return count, nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (*JobInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	info := r.toInfo()
	return &info, nil
}

func (s *MemoryStore) List(_ context.Context, f ListFilter) ([]JobInfo, string, error) {
	cursorTime, cursorID, err := DecodeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := NormalizeLimit(f.Limit)
	s.mu.Lock()
	defer s.mu.Unlock()

	var match []*memRow
	for _, r := range s.rows {
		if !rowMatches(r, f) {
			continue
		}
		if f.Cursor != "" {
			if r.createdAt.Before(cursorTime) || (r.createdAt.Equal(cursorTime) && r.id <= cursorID) {
				continue
			}
		}
		match = append(match, r)
	}
	slices.SortFunc(match, func(a, b *memRow) int {
		if c := a.createdAt.Compare(b.createdAt); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})

	var next string
	if len(match) > limit {
		last := match[limit-1]
		next = EncodeCursor(last.createdAt, last.id)
		match = match[:limit]
	}
	out := make([]JobInfo, len(match))
	for i, r := range match {
		out[i] = r.toInfo()
	}
	return out, next, nil
}

func (s *MemoryStore) ListAttempts(_ context.Context, jobID string, afterAttempt, limit int) ([]Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.attempts[jobID]
	out := make([]Attempt, 0, len(rows))
	for _, a := range rows {
		if a.Attempt <= afterAttempt {
			continue
		}
		out = append(out, a)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	slices.SortFunc(out, func(a, b Attempt) int { return cmp.Compare(a.Attempt, b.Attempt) })
	return out, nil
}

func (s *MemoryStore) Retry(_ context.Context, jobID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return ErrNotFound
	}
	if !r.state.Terminal() || r.state == StateSucceeded {
		return ErrJobNotRetryable
	}
	if r.UniqueKey != "" {
		k := uniqKey(r.Kind, r.HandlerID, r.UniqueKey)
		if holder, ok := s.uniq[k]; ok && holder != r.id {
			if other, ok := s.rows[holder]; ok && !other.state.Terminal() {
				return &DuplicateError{ExistingID: holder, Kind: r.Kind, HandlerID: r.HandlerID, UniqueKey: r.UniqueKey}
			}
		}
	}
	if r.attempt >= r.MaxAttempts {
		r.MaxAttempts = r.attempt + 1
	}
	r.state = StateAvailable
	r.AvailableAt = now
	r.lastError = ""
	r.lockedBy = ""
	r.lockedUntil = time.Time{}
	r.cancelRequested = false
	r.updatedAt = now
	if r.UniqueKey != "" {
		s.uniq[uniqKey(r.Kind, r.HandlerID, r.UniqueKey)] = r.id
	}
	return nil
}

func (s *MemoryStore) Cancel(_ context.Context, jobID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return false, ErrNotFound
	}
	if r.state.Terminal() {
		return false, ErrJobTerminal
	}
	if r.state == StateRunning {
		r.cancelRequested = true
		r.updatedAt = now
		return false, nil
	}
	r.state = StateCancelled
	r.updatedAt = now
	s.freeUniq(r)
	return true, nil
}

func (s *MemoryStore) Delete(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[jobID]
	if !ok {
		return ErrNotFound
	}
	if r.state == StateRunning {
		return ErrJobRunning
	}
	delete(s.rows, jobID)
	delete(s.attempts, jobID)
	s.freeUniq(r)
	return nil
}

func (s *MemoryStore) UpsertWorker(_ context.Context, w WorkerRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := w
	cp.Queues = append([]string(nil), w.Queues...)
	if prev, ok := s.workers[w.ID]; ok {
		cp.StartedAt = prev.StartedAt // preserve original start
	}
	s.workers[w.ID] = cp
	return nil
}

func (s *MemoryStore) RetireWorker(_ context.Context, workerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workers, workerID)
	return nil
}

func (s *MemoryStore) ListWorkers(_ context.Context) ([]WorkerRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WorkerRow, 0, len(s.workers))
	for _, w := range s.workers {
		cp := w
		cp.Queues = append([]string(nil), w.Queues...)
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b WorkerRow) int { return a.StartedAt.Compare(b.StartedAt) })
	return out, nil
}

func (s *MemoryStore) SweepStaleWorkers(_ context.Context, olderThan time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, w := range s.workers {
		if !w.LastSeenAt.IsZero() && w.LastSeenAt.Before(olderThan) {
			delete(s.workers, id)
			removed++
		}
	}
	return removed, nil
}

func (s *MemoryStore) ListQueues(_ context.Context) ([]QueueInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := map[string]map[State]int{}
	for _, r := range s.rows {
		m, ok := byName[r.Queue]
		if !ok {
			m = map[State]int{}
			byName[r.Queue] = m
		}
		m[r.state]++
	}
	out := make([]QueueInfo, 0, len(byName))
	for name, counts := range byName {
		cp := make(map[State]int, len(counts))
		for k, v := range counts {
			cp[k] = v
		}
		out = append(out, QueueInfo{Name: name, Counts: cp})
	}
	slices.SortFunc(out, func(a, b QueueInfo) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *MemoryStore) UpsertSchedule(_ context.Context, row ScheduleRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := schedKey(row.Group, row.Name)
	cp := row
	cp.Payload = append([]byte(nil), row.Payload...)
	cp.OptionsJSON = append([]byte(nil), row.OptionsJSON...)
	if prev, ok := s.schedules[key]; ok {
		cp.LastRunAt = prev.LastRunAt
		cp.LastRunSet = prev.LastRunSet
		if prev.Spec == row.Spec {
			cp.NextRunAt = prev.NextRunAt // preserve cadence
		}
	} else {
		cp.LastRunSet = false
	}
	s.schedules[key] = cp
	return nil
}

func (s *MemoryStore) DeleteSchedule(_ context.Context, group, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.schedules, schedKey(group, name))
	return nil
}

func (s *MemoryStore) ListSchedules(_ context.Context, group string) ([]ScheduleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScheduleRow
	for _, r := range s.schedules {
		if r.Group == group {
			out = append(out, cloneSchedule(r))
		}
	}
	slices.SortFunc(out, func(a, b ScheduleRow) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *MemoryStore) DueSchedules(_ context.Context, group string, now time.Time) ([]ScheduleRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ScheduleRow
	for _, r := range s.schedules {
		if r.Group == group && !r.NextRunAt.IsZero() && !r.NextRunAt.After(now) {
			out = append(out, cloneSchedule(r))
		}
	}
	slices.SortFunc(out, func(a, b ScheduleRow) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *MemoryStore) FireSchedule(_ context.Context, f ScheduleFire) (bool, []InsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := schedKey(f.Group, f.Name)
	sched, ok := s.schedules[key]
	if !ok {
		return false, nil, ErrNotFound
	}
	if sched.LastRunSet != f.ExpectedLastRun.Valid ||
		(f.ExpectedLastRun.Valid && !sched.LastRunAt.Equal(f.ExpectedLastRun.Time)) {
		return false, nil, nil
	}
	results := s.insertLocked(f.Now.UTC(), f.Jobs)
	sched.LastRunAt = f.NewLastRun
	sched.LastRunSet = !f.NewLastRun.IsZero()
	sched.NextRunAt = f.NewNextRun
	sched.UpdatedAt = f.Now.UTC()
	s.schedules[key] = sched
	return true, results, nil
}

func (s *MemoryStore) freeUniq(r *memRow) {
	if r.UniqueKey == "" {
		return
	}
	k := uniqKey(r.Kind, r.HandlerID, r.UniqueKey)
	if holder, ok := s.uniq[k]; ok && holder == r.id {
		delete(s.uniq, k)
	}
}

func (r *memRow) toInfo() JobInfo {
	return JobInfo{
		ID: r.id, Kind: r.Kind, HandlerID: r.HandlerID, Queue: r.Queue, Priority: r.Priority,
		State: r.state, Attempt: r.attempt, MaxAttempts: r.MaxAttempts, AvailableAt: r.AvailableAt,
		Timeout: time.Duration(r.TimeoutMs) * time.Millisecond, UniqueKey: r.UniqueKey,
		Payload: append([]byte(nil), r.Payload...), Error: r.lastError, CancelRequested: r.cancelRequested,
		ScheduledFor: r.ScheduledFor, ScheduledForSet: r.ScheduledForSet,
		CreatedAt: r.createdAt, UpdatedAt: r.updatedAt,
	}
}

func cloneSchedule(r ScheduleRow) ScheduleRow {
	cp := r
	cp.Payload = append([]byte(nil), r.Payload...)
	cp.OptionsJSON = append([]byte(nil), r.OptionsJSON...)
	return cp
}

func rowMatches(r *memRow, f ListFilter) bool {
	if len(f.Queues) > 0 && !slices.Contains(f.Queues, r.Queue) {
		return false
	}
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, r.Kind) {
		return false
	}
	if len(f.States) > 0 && !slices.Contains(f.States, r.state) {
		return false
	}
	return true
}
