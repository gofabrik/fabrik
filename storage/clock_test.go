package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMemoryClockInjectable(t *testing.T) {
	fixed := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	m := NewMemory()
	m.now = func() time.Time { return fixed }
	if err := m.Put(context.Background(), "k", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	info, err := m.Stat(context.Background(), "k")
	if err != nil || !info.ModTime.Equal(fixed) {
		t.Fatalf("ModTime = %v, want %v (err %v)", info.ModTime, fixed, err)
	}
}
