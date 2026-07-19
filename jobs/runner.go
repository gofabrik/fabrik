package jobs

import (
	"context"
	"time"
)

// Runner processes jobs until cancellation and drains in-flight work before returning.
type Runner struct {
	m   *Manager
	cfg RuntimeConfig
}

// NewRunner returns a Runner for the manager and runtime config.
func NewRunner(m *Manager, cfg RuntimeConfig) *Runner { return &Runner{m: m, cfg: cfg} }

// Run processes jobs until ctx is canceled, then drains for up to 30 seconds.
func (r *Runner) Run(ctx context.Context) error {
	drain, err := Run(ctx, r.m, r.cfg)
	if err != nil {
		return err
	}
	<-ctx.Done()
	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return drain(drainCtx)
}
