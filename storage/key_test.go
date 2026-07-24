package storage_test

import (
	"regexp"
	"testing"

	"github.com/gofabrik/fabrik/storage"
)

func TestUniqueKey_KeepsSafeNames(t *testing.T) {
	re := regexp.MustCompile(`^uploads/[0-9a-f]{32}-report~1.0_final\.pdf$`)
	key := storage.UniqueKey("uploads", "report~1.0_final.pdf")
	if !re.MatchString(key) {
		t.Fatalf("key = %q", key)
	}
	if err := storage.CheckKey(key); err != nil {
		t.Fatalf("CheckKey(%q) = %v", key, err)
	}
}

func TestUniqueKey_DropsUnsafeNames(t *testing.T) {
	re := regexp.MustCompile(`^uploads/[0-9a-f]{32}$`)
	for _, name := range []string{"", "/", "a b.txt", "q?.txt", "frag#.txt", "pct%20.txt", "über.png", `back\slash`} {
		key := storage.UniqueKey("uploads", name)
		if !re.MatchString(key) {
			t.Errorf("UniqueKey(uploads, %q) = %q, want the bare prefix", name, key)
		}
		if err := storage.CheckKey(key); err != nil {
			t.Errorf("CheckKey(%q) = %v", key, err)
		}
	}
}

func TestUniqueKey_EmptyDirAndUniqueness(t *testing.T) {
	a := storage.UniqueKey("", "a.txt")
	if !regexp.MustCompile(`^[0-9a-f]{32}-a\.txt$`).MatchString(a) {
		t.Fatalf("bare-dir key = %q", a)
	}
	if b := storage.UniqueKey("", "a.txt"); a == b {
		t.Fatalf("two keys collided: %q", a)
	}
}
