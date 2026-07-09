package config

import (
	"fmt"
	"sort"
	"strings"
)

// Problem is one field-level configuration failure. Key is a dotted YAML
// path or an env variable name.
type Problem struct {
	Key     string
	Message string
}

// LoadError aggregates [Problem] values found during [Load].
type LoadError struct {
	Problems []Problem
}

// Error renders problems sorted by key.
func (e *LoadError) Error() string {
	ps := append([]Problem(nil), e.Problems...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Key < ps[j].Key })

	noun := "problems"
	if len(ps) == 1 {
		noun = "problem"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: %d %s:", len(ps), noun)
	for _, p := range ps {
		fmt.Fprintf(&b, "\n  %s: %s", p.Key, p.Message)
	}
	return b.String()
}
