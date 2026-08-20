package mysql

import (
	"strings"
	"testing"
)

// TestLockNamePerDatabase pins the advisory-lock scoping: distinct
// databases get distinct lock names, and every name fits MySQL's
// 64-character lock-name limit.
func TestLockNamePerDatabase(t *testing.T) {
	a, b := lockName("app_one"), lockName("app_two")
	if a == b {
		t.Fatalf("lock names collide across databases: %q", a)
	}
	long := lockName(strings.Repeat("d", 300))
	for _, n := range []string{a, b, long} {
		if len(n) > 64 {
			t.Fatalf("lock name %q exceeds MySQL's 64-character limit", n)
		}
		if !strings.HasPrefix(n, lockPrefix) {
			t.Fatalf("lock name %q lost its prefix", n)
		}
	}
}
