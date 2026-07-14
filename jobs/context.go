package jobs

import (
	"context"
	"log/slog"
	"time"
)

// Context is the job-aware context a handler may take as its first
// parameter. Plain [context.Context] also works with the package
// accessors.
type Context interface {
	context.Context
	JobID() string
	Kind() string
	Attempt() int
	Logger() *slog.Logger
	ScheduledFor() (time.Time, bool)
}

// jobMeta is the per-run metadata carried on the context.
type jobMeta struct {
	jobID        string
	kind         string
	attempt      int
	logger       *slog.Logger
	scheduledFor time.Time
	scheduledSet bool
}

type metaKey struct{}

// jobCtx exposes metadata through Value so derived contexts keep accessors.
type jobCtx struct {
	context.Context
	meta *jobMeta
}

func (c *jobCtx) JobID() string        { return c.meta.jobID }
func (c *jobCtx) Kind() string         { return c.meta.kind }
func (c *jobCtx) Attempt() int         { return c.meta.attempt }
func (c *jobCtx) Logger() *slog.Logger { return c.meta.logger }

func (c *jobCtx) ScheduledFor() (time.Time, bool) {
	return c.meta.scheduledFor, c.meta.scheduledSet
}

func (c *jobCtx) Value(key any) any {
	if _, ok := key.(metaKey); ok {
		return c.meta
	}
	return c.Context.Value(key)
}

func metaFrom(ctx context.Context) *jobMeta {
	m, _ := ctx.Value(metaKey{}).(*jobMeta)
	return m
}

// JobID returns the running job's id, or "" outside a job.
func JobID(ctx context.Context) string {
	if m := metaFrom(ctx); m != nil {
		return m.jobID
	}
	return ""
}

// Kind returns the running job's message kind, or "" outside a job.
func Kind(ctx context.Context) string {
	if m := metaFrom(ctx); m != nil {
		return m.kind
	}
	return ""
}

// AttemptNumber returns the current attempt (1-based), or 0 outside a
// job. (Named to avoid colliding with the [Attempt] ledger type; the
// [Context.Attempt] method is unaffected.)
func AttemptNumber(ctx context.Context) int {
	if m := metaFrom(ctx); m != nil {
		return m.attempt
	}
	return 0
}

// Logger returns the per-job logger, or slog.Default outside a job.
func Logger(ctx context.Context) *slog.Logger {
	if m := metaFrom(ctx); m != nil && m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// ScheduledFor returns the tick a scheduler fire represents; ok is false
// for a directly enqueued job or outside a job.
func ScheduledFor(ctx context.Context) (time.Time, bool) {
	if m := metaFrom(ctx); m != nil {
		return m.scheduledFor, m.scheduledSet
	}
	return time.Time{}, false
}
