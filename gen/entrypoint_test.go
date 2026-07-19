package gen

import "testing"

func TestAddEntrypointDedupesUses(t *testing.T) {
	g := New()
	g.AddEntrypoint("JobWorker", []string{"mgr", "cfg", "mgr"}, []string{"return nil"})
	got := g.entrypoints[0].Uses
	want := []string{"mgr", "cfg"}
	if len(got) != len(want) {
		t.Fatalf("Uses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Uses = %v, want %v", got, want)
		}
	}
}
