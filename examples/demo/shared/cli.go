package shared

import (
	"fmt"

	"github.com/gofabrik/fabrik/cli"
)

//fabrik:cli:command
func ConfigCommand(cfg *Config, db *Database) *cli.Command {
	return &cli.Command{
		Name: "config",
		Help: "Print the resolved configuration",
		Run: func(ctx cli.Context) error {
			fmt.Fprintf(ctx.Stdout(), "http addr: %s\n", cfg.Addr)
			fmt.Fprintf(ctx.Stdout(), "database:  %s\n", db.Path)
			return nil
		},
	}
}
