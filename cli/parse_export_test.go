package cli_test

import (
	"testing"

	"github.com/gofabrik/fabrik/cli"
)

// This pins ParseResult usability outside package cli.
func TestParseResult_ExportedAPI(t *testing.T) {
	port := cli.IntFlag("port").Default(8080)
	serve := &cli.Command{Name: "serve", Flags: cli.Flags(port), Run: func(cli.Context) error { return nil }}
	root := &cli.Command{Name: "myapp", Version: "1.0.0", Subcommands: []*cli.Command{serve}}

	res, err := root.Parse([]string{"serve", "--port", "9090"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command() != serve {
		t.Errorf("Command(): resolved wrong command")
	}
	if res.Help() || res.Version() {
		t.Errorf("Help()/Version() should be false for a normal parse")
	}
	if got := port.Get(res.Context()); got != 9090 {
		t.Errorf("port via Context(): want 9090, got %d", got)
	}

	if h, err := root.Parse([]string{"--help"}); err != nil || !h.Help() {
		t.Errorf("Help() should be true for --help (err=%v)", err)
	}
	if v, err := root.Parse([]string{"--version"}); err != nil || !v.Version() {
		t.Errorf("Version() should be true for --version (err=%v)", err)
	}
}
