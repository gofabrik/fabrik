package session_test

import (
	"testing"

	"github.com/gofabrik/fabrik/session"
	"github.com/gofabrik/fabrik/session/storetest"
)

func TestMemoryStoreConformance(t *testing.T) {
	storetest.Run(t, func() session.Store { return session.NewMemoryStore() })
}
