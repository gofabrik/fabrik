package cache

import (
	"context"
	"errors"
	"sync"
)

// loadGroup provides dependency-free keyed call deduplication.
type loadGroup struct {
	mu    sync.Mutex
	calls map[string]*activeLoad
}

type activeLoad struct {
	done      chan struct{}
	val       any
	err       error
	runnerCtx error // the running caller's ctx.Err() when the load ended
	panicked  any

	// pubSem serializes publishing with invalidation. A channel makes
	// invalidation cancellable.
	pubSem      chan struct{}
	invalidated bool
}

func newLoadGroup() *loadGroup {
	return &loadGroup{calls: make(map[string]*activeLoad)}
}

// retryable permits a caller with a live context to take over a load
// whose running caller was canceled. A cancellation error returned by
// the function itself propagates.
func retryable(callErr, runnerCtx, callerCtx error) bool {
	return callErr != nil && runnerCtx != nil && errors.Is(callErr, runnerCtx) && callerCtx == nil
}

// invalidate prevents an active load from storing stale data. It
// waits for a store write already under way so the subsequent
// deletion wins, or returns ctx.Err() if the wait expires.
func (g *loadGroup) invalidate(ctx context.Context, key string) error {
	g.mu.Lock()
	c, ok := g.calls[key]
	g.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case c.pubSem <- struct{}{}:
		c.invalidated = true
		<-c.pubSem
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Do runs fn once per key; concurrent callers share the result. A
// waiting caller returns its own context error, and takes over when
// the running caller's cancellation ended the load. The publish guard
// rejects invalidated writes and serializes admitted writes with
// invalidation. Panics propagate to every caller.
func (g *loadGroup) Do(ctx context.Context, key string, fn func(publish func(write func()) bool) (any, error)) (any, error) {
	for {
		v, err, retry := g.once(ctx, key, fn)
		if !retry {
			return v, err
		}
	}
}

func (g *loadGroup) once(ctx context.Context, key string, fn func(publish func(write func()) bool) (any, error)) (v any, err error, retry bool) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			if c.panicked != nil {
				panic(c.panicked)
			}
			// The caller's own cancellation wins even when completion
			// is also ready.
			if err := ctx.Err(); err != nil {
				return nil, err, false
			}
			if retryable(c.err, c.runnerCtx, nil) {
				return nil, nil, true
			}
			return c.val, c.err, false
		case <-ctx.Done():
			return nil, ctx.Err(), false
		}
	}
	c := &activeLoad{done: make(chan struct{}), pubSem: make(chan struct{}, 1)}
	g.calls[key] = c
	g.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			c.panicked = r
		}
		c.runnerCtx = ctx.Err()
		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()
		close(c.done)
		if c.panicked != nil {
			panic(c.panicked)
		}
	}()
	c.val, c.err = fn(c.publish)
	return c.val, c.err, false
}

// publish makes write admission atomic with invalidation.
func (c *activeLoad) publish(write func()) bool {
	c.pubSem <- struct{}{}
	defer func() { <-c.pubSem }()
	if c.invalidated {
		return false
	}
	write()
	return true
}
