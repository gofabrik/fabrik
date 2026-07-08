// Package diag defines fabrik's shared diagnostic model.
package diag

import (
	"go/token"
	"sort"
)

// Severity classifies a diagnostic.
type Severity int

const (
	SevError Severity = iota
	SevWarning
)

// Diagnostic is one positioned problem with optional help.
type Diagnostic struct {
	Severity Severity
	Pos      token.Position
	Message  string
	Help     string
}

// Diagnostics is an ordered collection of diagnostics.
type Diagnostics []Diagnostic

// HasFatal reports whether any diagnostic is an error.
func (ds Diagnostics) HasFatal() bool {
	for _, d := range ds {
		if d.Severity == SevError {
			return true
		}
	}
	return false
}

// Counts returns the number of errors and warnings.
func (ds Diagnostics) Counts() (errs, warns int) {
	for _, d := range ds {
		if d.Severity == SevError {
			errs++
		} else {
			warns++
		}
	}
	return
}

// Sort orders diagnostics by file, line, then column for stable output.
func (ds Diagnostics) Sort() {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i].Pos, ds[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
}

// Error appends an error diagnostic.
func (ds *Diagnostics) Error(pos token.Position, msg, help string) {
	*ds = append(*ds, Diagnostic{Severity: SevError, Pos: pos, Message: msg, Help: help})
}

// Warn appends a warning diagnostic.
func (ds *Diagnostics) Warn(pos token.Position, msg, help string) {
	*ds = append(*ds, Diagnostic{Severity: SevWarning, Pos: pos, Message: msg, Help: help})
}
