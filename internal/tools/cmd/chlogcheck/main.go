// Command chlogcheck validates chloggen changelog fragments without requiring issue links.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/gofabrik/fabrik/internal/tools/chlogcheck"
)

func main() {
	cfg := flag.String("config", ".chloggen/config.yaml", "path to the chloggen config file")
	flag.Parse()
	if err := chlogcheck.Validate(*cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("PASS: changelog fragments are valid")
}
