package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadSharesOneCall(t *testing.T) {
	g := newLoadGroup()
	var runs atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := g.Do(context.Background(), "k", func(func(func()) bool) (any, error) {
				runs.Add(1)
				<-release
				return "shared", nil
			})
			if err != nil || v != "shared" {
				t.Errorf("Do = %v, %v", v, err)
			}
		}()
	}
	for runs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1", runs.Load())
	}
}

func TestLoadWaitingCallerLeavesOnItsOwnCancel(t *testing.T) {
	g := newLoadGroup()
	started := make(chan struct{})
	release := make(chan struct{})
	runnerDone := make(chan error, 1)
	go func() {
		_, err := g.Do(context.Background(), "k", func(func(func()) bool) (any, error) {
			close(started)
			<-release
			return "v", nil
		})
		runnerDone <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	_, err := g.Do(ctx, "k", func(func(func()) bool) (any, error) { panic("a second load must not run") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting caller err = %v", err)
	}
	close(release)
	if err := <-runnerDone; err != nil {
		t.Fatalf("running caller err = %v", err)
	}
}

func TestLoadCanceledRunnerFailsAlone(t *testing.T) {
	g := newLoadGroup()
	runnerCtx, cancelRunner := context.WithCancel(context.Background())
	runnerIn := make(chan struct{})
	var runs atomic.Int32
	fn := func(func(func()) bool) (any, error) {
		if runs.Add(1) == 1 {
			close(runnerIn)
			<-runnerCtx.Done()
			return nil, runnerCtx.Err()
		}
		return "recomputed", nil
	}
	runnerErr := make(chan error, 1)
	go func() {
		_, err := g.Do(runnerCtx, "k", fn)
		runnerErr <- err
	}()
	<-runnerIn
	callerDone := make(chan struct{})
	go func() {
		defer close(callerDone)
		v, err := g.Do(context.Background(), "k", fn)
		if err != nil || v != "recomputed" {
			t.Errorf("live caller after runner cancel: %v %v", v, err)
		}
	}()
	time.Sleep(5 * time.Millisecond)
	cancelRunner()
	if err := <-runnerErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled running caller err = %v", err)
	}
	<-callerDone
	if runs.Load() != 2 {
		t.Fatalf("runs = %d, want 2 (takeover)", runs.Load())
	}
}

func TestLoadSelfCanceledFuncPropagates(t *testing.T) {
	g := newLoadGroup()
	in := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	go func() {
		g.Do(context.Background(), "k", func(func(func()) bool) (any, error) { //nolint:errcheck
			runs.Add(1)
			close(in)
			<-release
			return nil, context.Canceled
		})
	}()
	<-in
	res := make(chan error, 1)
	go func() {
		_, err := g.Do(context.Background(), "k", func(func(func()) bool) (any, error) {
			runs.Add(1)
			return nil, nil
		})
		res <- err
	}()
	time.Sleep(5 * time.Millisecond)
	close(release)
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting caller err = %v, want the function's Canceled", err)
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d: a function-returned Canceled with a live runner must not trigger takeover", runs.Load())
	}
}

func TestLoadDeadlineExpiredRunnerTakeover(t *testing.T) {
	g := newLoadGroup()
	runnerCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	in := make(chan struct{})
	var runs atomic.Int32
	fn := func(func(func()) bool) (any, error) {
		if runs.Add(1) == 1 {
			close(in)
			<-runnerCtx.Done()
			return nil, runnerCtx.Err()
		}
		return "took over", nil
	}
	go g.Do(runnerCtx, "k", fn) //nolint:errcheck
	<-in
	v, err := g.Do(context.Background(), "k", fn)
	if err != nil || v != "took over" || runs.Load() != 2 {
		t.Fatalf("takeover = %v %v runs=%d", v, err, runs.Load())
	}
}

func TestLoadPanicPropagates(t *testing.T) {
	g := newLoadGroup()
	in := make(chan struct{})
	saw := make(chan any, 2)
	go func() {
		defer func() { saw <- recover() }()
		g.Do(context.Background(), "k", func(func(func()) bool) (any, error) { //nolint:errcheck
			close(in)
			time.Sleep(5 * time.Millisecond)
			panic("boom")
		})
	}()
	<-in
	func() {
		defer func() { saw <- recover() }()
		g.Do(context.Background(), "k", func(func(func()) bool) (any, error) { return nil, nil }) //nolint:errcheck
	}()
	if a, b := <-saw, <-saw; a != "boom" || b != "boom" {
		t.Fatalf("panic propagation: %v %v", a, b)
	}
}

// A live caller retries only when the runner's own context ended the load.
func TestRetryClassification(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name                          string
		callErr, runnerCtx, callerCtx error
		want                          bool
	}{
		{"runner canceled, live caller", context.Canceled, context.Canceled, nil, true},
		{"runner deadline, live caller", context.DeadlineExceeded, context.DeadlineExceeded, nil, true},
		{"func self-canceled, live runner", context.Canceled, nil, nil, false},
		{"runner canceled, caller canceled too", context.Canceled, context.Canceled, context.Canceled, false},
		{"plain error", boom, nil, nil, false},
		{"plain error with dead runner", boom, context.Canceled, nil, false},
		{"success", nil, context.Canceled, nil, false},
	}
	for _, c := range cases {
		if got := retryable(c.callErr, c.runnerCtx, c.callerCtx); got != c.want {
			t.Errorf("%s: retryable = %v, want %v", c.name, got, c.want)
		}
	}
}

// A canceled caller gets its context error even if completion is ready.
func TestLoadCanceledCallerNeverGetsResult(t *testing.T) {
	for i := 0; i < 100; i++ {
		g := newLoadGroup()
		started := make(chan struct{})
		release := make(chan struct{})
		go g.Do(context.Background(), "k", func(func(func()) bool) (any, error) { //nolint:errcheck
			close(started)
			<-release
			return "v", nil
		})
		<-started
		ctx, cancel := context.WithCancel(context.Background())
		caller := make(chan error, 1)
		go func() {
			// A late caller may run the load, but its canceled context still wins.
			_, err := g.Do(ctx, "k", func(func(func()) bool) (any, error) { return nil, ctx.Err() })
			caller <- err
		}()
		time.Sleep(time.Millisecond)
		cancel()
		close(release)
		if err := <-caller; !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: canceled caller got %v", i, err)
		}
	}
}

func TestLoadKeyFreedAfterCompletion(t *testing.T) {
	g := newLoadGroup()
	if _, err := g.Do(context.Background(), "k", func(func(func()) bool) (any, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	v, err := g.Do(context.Background(), "k", func(func(func()) bool) (any, error) { return 2, nil })
	if err != nil || v != 2 {
		t.Fatalf("second load = %v, %v", v, err)
	}
}
