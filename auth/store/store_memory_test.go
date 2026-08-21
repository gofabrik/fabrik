package store_test

import (
	"testing"
	"time"

	"github.com/gofabrik/fabrik/auth/store"
	"github.com/gofabrik/fabrik/auth/store/storetest"
)

func TestMemoryStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T, now func() time.Time) store.Store {
		return store.NewMemoryStore(store.MemoryOptions{Now: now})
	})
}
