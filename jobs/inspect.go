package jobs

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// JobInfo is the inspection view returned by [Manager.GetJob] and [Manager.ListJobs].
type JobInfo struct {
	ID          string
	Kind        string
	HandlerID   string
	Queue       string
	Priority    int
	State       State
	Attempt     int
	MaxAttempts int
	AvailableAt time.Time
	Timeout     time.Duration
	UniqueKey   string
	// Payload is the raw JSON of the message; a defensive copy.
	Payload         []byte
	Error           string
	CancelRequested bool
	ScheduledFor    time.Time
	ScheduledForSet bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// JobPage is one page of [Manager.ListJobs].
type JobPage struct {
	Jobs       []JobInfo
	NextCursor string
}

const (
	// DefaultListLimit is the page size when a filter names none.
	DefaultListLimit = 100
	// MaxListLimit caps a single page.
	MaxListLimit = 1000
)

// NormalizeLimit applies the default and cap for [Store.List].
func NormalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultListLimit
	}
	if limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

// EncodeCursor returns the opaque list cursor for (createdAt, id).
func EncodeCursor(createdAt time.Time, id string) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a list cursor. Empty input yields a zero time and
// empty id ("no cursor").
func DecodeCursor(c string) (time.Time, string, error) {
	if c == "" {
		return time.Time{}, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("jobs: invalid cursor: %w", err)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("jobs: malformed cursor")
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("jobs: cursor timestamp: %w", err)
	}
	return time.Unix(0, nanos).UTC(), parts[1], nil
}

// GetJob returns one job, or [ErrNotFound].
func (m *Manager) GetJob(ctx context.Context, id string) (*JobInfo, error) {
	return m.store.Get(ctx, id)
}

// ListJobs returns a page of jobs matching the filter.
func (m *Manager) ListJobs(ctx context.Context, f ListFilter) (JobPage, error) {
	rows, next, err := m.store.List(ctx, f)
	if err != nil {
		return JobPage{}, err
	}
	return JobPage{Jobs: rows, NextCursor: next}, nil
}

// ListJobAttempts returns a job's attempt ledger, oldest first.
func (m *Manager) ListJobAttempts(ctx context.Context, jobID string) ([]Attempt, error) {
	return m.store.ListAttempts(ctx, jobID, 0, MaxListLimit)
}

// ListQueues returns one entry per queue with per-state counts.
func (m *Manager) ListQueues(ctx context.Context) ([]QueueInfo, error) {
	return m.store.ListQueues(ctx)
}

// ListWorkers returns the currently-alive workers.
func (m *Manager) ListWorkers(ctx context.Context) ([]WorkerRow, error) {
	return m.store.ListWorkers(ctx)
}
