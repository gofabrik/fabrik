package jobs

import "context"

// RetryJob revives a terminal job (failed, discarded, cancelled) to
// available, preserving the attempt count and bumping the cap by one
// when at it. Refuses succeeded and non-terminal states with
// [ErrJobNotRetryable]; a live UniqueKey collision returns a
// [*DuplicateError].
func (m *Manager) RetryJob(ctx context.Context, id string) error {
	return m.store.Retry(ctx, id, m.now())
}

// CancelJob requests cancellation. A pending/available job transitions
// to cancelled immediately (immediate=true); a running job has a flag
// set that the worker observes on its next heartbeat (immediate=false);
// a terminal job returns [ErrJobTerminal].
func (m *Manager) CancelJob(ctx context.Context, id string) (immediate bool, err error) {
	return m.store.Cancel(ctx, id, m.now())
}

// DeleteJob removes a job and its attempts. Returns [ErrJobRunning] for
// a leased job; cancel first and wait for the lease to release.
func (m *Manager) DeleteJob(ctx context.Context, id string) error {
	return m.store.Delete(ctx, id)
}
