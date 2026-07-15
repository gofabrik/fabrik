package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// CatchUpAllMax caps the number of fires one [CatchUpAll] tick produces.
const CatchUpAllMax = 1000

// Spec is a schedule trigger: [Cron] or [Every]. The zero value is
// invalid.
type Spec struct {
	cron  string
	every time.Duration
}

// Cron is a five-field standard cron expression.
func Cron(expr string) Spec { return Spec{cron: expr} }

// Every is a fixed interval.
func Every(d time.Duration) Spec { return Spec{every: d} }

// String is the persisted form of a spec.
func (s Spec) String() string {
	if s.every > 0 {
		return "every:" + strconv.FormatInt(int64(s.every), 10)
	}
	return "cron:" + s.cron
}

func parseSpec(raw string) (Spec, error) {
	if rest, ok := strings.CutPrefix(raw, "every:"); ok {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return Spec{}, fmt.Errorf("jobs: bad every spec %q: %w", raw, err)
		}
		return Spec{every: time.Duration(n)}, nil
	}
	if rest, ok := strings.CutPrefix(raw, "cron:"); ok {
		return Spec{cron: rest}, nil
	}
	return Spec{}, fmt.Errorf("jobs: unknown schedule spec %q", raw)
}

// validate rejects a bad cron expression or a non-positive interval.
func (s Spec) validate() error {
	if s.every > 0 {
		return nil
	}
	if s.every < 0 {
		return fmt.Errorf("jobs: Every interval must be positive")
	}
	if s.cron == "" {
		return fmt.Errorf("jobs: empty schedule spec")
	}
	if _, err := cron.ParseStandard(s.cron); err != nil {
		return fmt.Errorf("jobs: bad cron %q: %w", s.cron, err)
	}
	return nil
}

// next returns the first tick strictly after t, in loc's wall clock.
func (s Spec) next(t time.Time, loc *time.Location) (time.Time, error) {
	if s.every > 0 {
		return t.Add(s.every).UTC(), nil
	}
	sched, err := cron.ParseStandard(s.cron)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(t.In(loc)).UTC(), nil
}

// ScheduleOptions controls a scheduled enqueue. Backoff and UniqueKey
// are absent: Singleton owns uniqueness, backoff stays the manager
// default.
type ScheduleOptions struct {
	Queue       string        `json:"queue,omitempty"`
	Priority    int           `json:"priority,omitempty"`
	MaxAttempts int           `json:"max_attempts,omitempty"`
	Timeout     time.Duration `json:"timeout,omitempty"`
	OnTimeout   OnTimeout     `json:"on_timeout,omitempty"`
	Singleton   bool          `json:"singleton,omitempty"`
	CatchUp     CatchUp       `json:"catch_up,omitempty"`
}

// Schedule declares a recurring message under (SchedulerGroup, name).
// Declarations are in-memory until [Manager.ReconcileSchedules] persists
// them. For one-off delayed work, use Enqueue with [At] or [After].
func (m *Manager) Schedule(name string, spec Spec, msg any, opts ScheduleOptions) error {
	if err := validIdent("schedule name", name); err != nil {
		return err
	}
	if err := spec.validate(); err != nil {
		return err
	}
	kind, ids, err := m.resolve(msg)
	if err != nil {
		return err
	}
	// Firing uses the live handler set, so handler ids are not persisted.
	if len(ids) == 0 {
		return fmt.Errorf("jobs.Schedule: kind %q has no handler; a schedule that fires nothing is refused", kind)
	}
	// Reject declarations that would fail on every scheduler tick.
	if opts.Queue != "" {
		if err := validIdent("queue", opts.Queue); err != nil {
			return err
		}
	}
	if opts.MaxAttempts < 0 {
		return fmt.Errorf("jobs.Schedule: MaxAttempts must be >= 0 (got %d)", opts.MaxAttempts)
	}
	if opts.Timeout < 0 {
		return fmt.Errorf("jobs.Schedule: Timeout must be >= 0 (got %v)", opts.Timeout)
	}
	if err := validOnTimeout(opts.OnTimeout); err != nil {
		return err
	}
	if err := validCatchUp(opts.CatchUp); err != nil {
		return err
	}
	if opts.Singleton && opts.CatchUp == CatchUpAll {
		m.config.Logger.Warn("jobs.Schedule: Singleton with CatchUpAll collapses every catch-up fire to one live job; use CatchUpOnce for the same effect without redundant inserts",
			"schedule", name)
	}
	payload, err := encodePayload(msg)
	if err != nil {
		return err
	}
	row, err := m.buildScheduleRow(name, kind, payload, spec, opts)
	if err != nil {
		return err
	}
	return m.declare(row)
}

func (m *Manager) buildScheduleRow(name, kind string, payload []byte, spec Spec, opts ScheduleOptions) (ScheduleRow, error) {
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		return ScheduleRow{}, fmt.Errorf("jobs: encode schedule options: %w", err)
	}
	now := m.now()
	first, err := spec.next(now, m.config.Location)
	if err != nil {
		return ScheduleRow{}, err
	}
	return ScheduleRow{
		Group:       m.config.SchedulerGroup,
		Name:        name,
		Kind:        kind,
		Spec:        spec.String(),
		Payload:     payload,
		OptionsJSON: optsJSON,
		NextRunAt:   first,
		UpdatedAt:   now,
	}, nil
}

// Schedule names are persistent identities within a scheduler group.
func (m *Manager) declare(row ScheduleRow) error {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	if _, dup := m.declared[row.Name]; dup {
		return fmt.Errorf("%w: %q", ErrScheduleAlreadyDeclared, row.Name)
	}
	m.declared[row.Name] = row
	return nil
}

// ReconcileSchedules syncs this process's declared schedules to the store
// for its group, upserting declarations and deleting orphaned rows.
func (m *Manager) ReconcileSchedules(ctx context.Context) error {
	m.schedMu.Lock()
	declared := make([]ScheduleRow, 0, len(m.declared))
	names := make(map[string]struct{}, len(m.declared))
	for name, row := range m.declared {
		declared = append(declared, row)
		names[name] = struct{}{}
	}
	m.schedMu.Unlock()

	sort.Slice(declared, func(i, j int) bool { return declared[i].Name < declared[j].Name })
	for _, row := range declared {
		if err := m.store.UpsertSchedule(ctx, row); err != nil {
			return err
		}
	}

	rows, err := m.store.ListSchedules(ctx, m.config.SchedulerGroup)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, ok := names[r.Name]; ok {
			continue
		}
		if err := m.store.DeleteSchedule(ctx, m.config.SchedulerGroup, r.Name); err != nil {
			return err
		}
		m.config.Logger.Info("jobs: pruned orphan schedule", "group", m.config.SchedulerGroup, "schedule", r.Name)
	}
	return nil
}

// StartScheduler blocks and fires due schedules from the store. Call
// [Manager.ReconcileSchedules] first after the schedule schema exists.
// Safe on multiple processes: [Store.FireSchedule] picks one winner per
// tick. Returns ctx.Err() on cancel.
func (m *Manager) StartScheduler(ctx context.Context) error {
	interval := m.config.SchedulerInterval
	if interval <= 0 {
		interval = 1 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.schedulerTick(ctx)
		}
	}
}

func (m *Manager) schedulerTick(ctx context.Context) {
	now := m.now()
	dueCtx, cancel := m.withStoreTimeout(ctx)
	due, err := m.store.DueSchedules(dueCtx, m.config.SchedulerGroup, now)
	cancel()
	if err != nil {
		m.config.Logger.Warn("jobs: scheduler DueSchedules failed", "err", err)
		return
	}
	for _, row := range due {
		m.fireSchedule(ctx, row, now)
	}
}

func (m *Manager) fireSchedule(ctx context.Context, row ScheduleRow, now time.Time) {
	spec, err := parseSpec(row.Spec)
	if err != nil {
		m.config.Logger.Warn("jobs: scheduler bad spec", "schedule", row.Name, "err", err)
		return
	}
	var opts ScheduleOptions
	if err := json.Unmarshal(row.OptionsJSON, &opts); err != nil {
		m.config.Logger.Warn("jobs: scheduler bad options", "schedule", row.Name, "err", err)
		return
	}
	ticks, next, capped, err := planFires(spec, m.config.Location, row.NextRunAt, now, opts.CatchUp)
	if err != nil {
		m.config.Logger.Warn("jobs: scheduler planFires", "schedule", row.Name, "err", err)
		return
	}
	if capped {
		m.config.Logger.Warn("jobs: scheduler CatchUpAll capped", "schedule", row.Name, "fires", len(ticks), "cap", CatchUpAllMax)
	}

	expected := sql.NullTime{Time: row.LastRunAt, Valid: row.LastRunSet}

	if len(ticks) == 0 {
		m.advanceSchedule(ctx, row, next, expected)
		return
	}

	// Fan-out follows handlers registered in this scheduler process.
	m.mu.RLock()
	handlerIDs := append([]string(nil), m.kindHandlers[row.Kind]...)
	m.mu.RUnlock()
	if len(handlerIDs) == 0 {
		m.config.Logger.Warn("jobs: scheduler kind has no handler here", "schedule", row.Name, "kind", row.Kind)
		return
	}

	o := &jobOpts{
		queue:       opts.Queue,
		priority:    opts.Priority,
		maxAttempts: opts.MaxAttempts,
		timeout:     opts.Timeout,
		onTimeout:   opts.OnTimeout,
	}
	if opts.Singleton {
		o.uniqueKey = "schedule:" + row.Group + ":" + row.Name
	}

	var emitted []Job
	for _, tick := range ticks {
		jobsForTick, err := m.buildJobs(row.Kind, handlerIDs, json.RawMessage(row.Payload), o, tick, true)
		if err != nil {
			// Skip invalid persisted rows without creating a hot loop.
			m.config.Logger.Warn("jobs: scheduler buildJobs; advancing without firing", "schedule", row.Name, "err", err)
			m.advanceSchedule(ctx, row, next, expected)
			return
		}
		emitted = append(emitted, jobsForTick...)
	}

	fire := ScheduleFire{Group: row.Group, Name: row.Name, ExpectedLastRun: expected, NewLastRun: now, NewNextRun: next, Now: now, Jobs: emitted}
	fCtx, cancel := m.withStoreTimeout(ctx)
	won, results, err := m.store.FireSchedule(fCtx, fire)
	cancel()
	if err != nil {
		m.config.Logger.Warn("jobs: scheduler FireSchedule", "schedule", row.Name, "err", err)
		return
	}
	if !won {
		return
	}
	// Results preserve each catch-up tick for enqueue hooks.
	m.fireEnqueueHooks(ctx, results, row.Kind, false, row.Name)
}

// advanceSchedule skips a tick without recording a run or firing enqueue hooks.
func (m *Manager) advanceSchedule(ctx context.Context, row ScheduleRow, next time.Time, expected sql.NullTime) {
	fire := ScheduleFire{Group: row.Group, Name: row.Name, ExpectedLastRun: expected, NewLastRun: row.LastRunAt, NewNextRun: next, Now: m.now()}
	if !row.LastRunSet {
		fire.NewLastRun = time.Time{} // keep last_run NULL
	}
	fCtx, cancel := m.withStoreTimeout(ctx)
	_, _, _ = m.store.FireSchedule(fCtx, fire)
	cancel()
}

// planFires returns the tick times this fire should emit, the new
// NextRunAt to persist, and whether the CatchUpAll cap was hit.
func planFires(spec Spec, loc *time.Location, nextRun, now time.Time, catchUp CatchUp) (ticks []time.Time, next time.Time, capped bool, err error) {
	switch catchUp {
	case CatchUpSkip:
		if nextRun.After(now) {
			return nil, nextRun, false, nil
		}
		next, err = spec.next(now, loc)
		return nil, next, false, err
	case CatchUpAll:
		t := nextRun
		for !t.After(now) {
			if len(ticks) >= CatchUpAllMax {
				next, err = spec.next(now, loc)
				return ticks, next, true, err
			}
			ticks = append(ticks, t)
			t, err = spec.next(t, loc)
			if err != nil {
				return nil, time.Time{}, false, err
			}
		}
		return ticks, t, false, nil
	default: // CatchUpOnce
		if nextRun.After(now) {
			return nil, nextRun, false, nil
		}
		next, err = spec.next(now, loc)
		return []time.Time{nextRun}, next, false, err
	}
}
