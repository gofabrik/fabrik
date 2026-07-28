package assetmapper

import (
	"slices"
	"testing"
)

func TestDependencyComponentsOrdersDependenciesAndIdentifiesCycles(t *testing.T) {
	deps := map[string][]string{
		"entry.js": {"a.js"},
		"a.js":     {"b.js"},
		"b.js":     {"a.js"},
		"plain.js": nil,
		"self.js":  {"self.js"},
	}
	got := dependencyComponents(deps)
	wantNodes := [][]string{
		{"a.js", "b.js"},
		{"entry.js"},
		{"plain.js"},
		{"self.js"},
	}
	wantCyclic := []bool{true, false, false, true}
	if len(got) != len(wantNodes) {
		t.Fatalf("components = %v, want %v", got, wantNodes)
	}
	for i := range got {
		if !slices.Equal(got[i].nodes, wantNodes[i]) || got[i].cyclic != wantCyclic[i] {
			t.Errorf("component %d = %+v, want nodes %v cyclic %t", i, got[i], wantNodes[i], wantCyclic[i])
		}
	}
}

func TestCyclicComponentHashIncludesOriginalSourceBytes(t *testing.T) {
	const source = "placeholder"
	marker := []byte("\x00assetmapper-scc:b.js")
	members := map[string]struct{}{"a.js": {}, "b.js": {}}
	nodes := []string{"a.js", "b.js"}
	first := cyclicComponentHash(
		nodes,
		map[string]*collectedAsset{
			"a.js": {content: []byte(source)},
			"b.js": {content: []byte("fixed")},
		},
		map[string][]ref{
			"a.js": {{
				spec:     source,
				resolved: "b.js",
				start:    0,
				end:      len(source),
			}},
		},
		members,
		"/assets/",
		nil,
	)
	second := cyclicComponentHash(
		nodes,
		map[string]*collectedAsset{
			"a.js": {content: marker},
			"b.js": {content: []byte("fixed")},
		},
		nil,
		members,
		"/assets/",
		nil,
	)
	if first == second {
		t.Fatal("different source bytes produced the same canonical component digest")
	}
}
