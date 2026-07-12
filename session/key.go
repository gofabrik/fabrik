package session

import (
	"fmt"
	"reflect"
)

// appKey is reserved for the app's own session data.
const appKey = "app"

// Key names one typed session cell for library code. Declare one per
// cell, usually unexported and named by the owning module path:
//
//	type data struct{ Token string }
//
//	var key = session.NewKey[data]("github.com/gofabrik/fabrik/csrf")
//
// Exporting a key deliberately shares its cell.
type Key[T any] struct {
	name string
}

// Name returns the cell name the key was declared with.
func (k Key[T]) Name() string { return k.name }

// NewKey declares a typed cell key. It panics on empty or reserved
// names.
func NewKey[T any](name string) Key[T] {
	if name == "" {
		panic("session: NewKey called with an empty name")
	}
	if name == appKey {
		panic(fmt.Sprintf("session: NewKey called with the reserved name %q (the app's own cell)", appKey))
	}
	return Key[T]{name: name}
}

// checkCellType validates the struct payload invariant.
func checkCellType(t reflect.Type, fn string) error {
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("session.%s: cell type %s is not a struct", fn, typeLabel(t))
	}
	return nil
}

func typeLabel(t reflect.Type) string {
	if t.Name() != "" && t.PkgPath() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}
