package shared

import (
	"context"
	"errors"
	"testing"
)

func TestSuperviseRuntimesCancelsPeerAfterCleanExit(t *testing.T) {
	peerStarted := make(chan struct{})
	peerStopped := make(chan struct{})
	clean := func(context.Context) error {
		<-peerStarted
		return nil
	}
	peer := func(ctx context.Context) error {
		close(peerStarted)
		<-ctx.Done()
		close(peerStopped)
		return ctx.Err()
	}

	if err := superviseRuntimes(context.Background(), clean, peer); err != nil {
		t.Fatalf("superviseRuntimes: %v", err)
	}
	select {
	case <-peerStopped:
	default:
		t.Fatal("peer did not stop after the clean runtime exited")
	}
}

func TestSuperviseRuntimesReturnsRuntimeError(t *testing.T) {
	want := errors.New("runtime failed")
	peerStarted := make(chan struct{})
	failing := func(context.Context) error {
		<-peerStarted
		return want
	}
	peer := func(ctx context.Context) error {
		close(peerStarted)
		<-ctx.Done()
		return ctx.Err()
	}

	if err := superviseRuntimes(context.Background(), failing, peer); !errors.Is(err, want) {
		t.Fatalf("superviseRuntimes error = %v, want %v", err, want)
	}
}

func TestSuperviseRuntimesIgnoresCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wait := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	if err := superviseRuntimes(ctx, wait, wait); err != nil {
		t.Fatalf("superviseRuntimes cancellation = %v, want nil", err)
	}
}
