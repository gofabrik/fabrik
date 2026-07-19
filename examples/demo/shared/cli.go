package shared

import (
	"context"
	"errors"
	"fmt"

	"github.com/gofabrik/fabrik/cli"
	"github.com/gofabrik/fabrik/httpserver"
	"github.com/gofabrik/fabrik/jobs"
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

// RunCommand returns a command that runs the HTTP server and jobs worker together.
//
//fabrik:cli:command
func RunCommand(server *httpserver.Server, worker *jobs.Runner) *cli.Command {
	return &cli.Command{
		Name: "run",
		Help: "Start the HTTP server and background worker",
		Run: func(ctx cli.Context) error {
			ectx, ecancel := context.WithCancel(ctx)
			defer ecancel()
			errc := make(chan error, 2)
			go func() { errc <- server.Run(ectx) }()
			go func() { errc <- worker.Run(ectx) }()
			var result error
			for range 2 {
				if e := <-errc; e != nil && !errors.Is(e, context.Canceled) && result == nil {
					result = e
					ecancel()
				}
			}
			return result
		},
	}
}

//fabrik:cli:command
func ServeCommand(server *httpserver.Server) *cli.Command {
	return &cli.Command{
		Name: "serve",
		Help: "Start only the HTTP server",
		Run:  func(ctx cli.Context) error { return server.Run(ctx) },
	}
}
