package main

import (
	"fmt"

	"github.com/gofabrik/fabrik/fabrik/internal/engine"
)

// directivesCmd prints the registry-backed directive reference.
func directivesCmd(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fabrik directives")
	}
	fmt.Print(engine.DirectivesDoc())
	return nil
}
