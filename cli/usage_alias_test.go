package cli

import (
	"strings"
	"testing"
)

func migrateCmd(usage string) *Command {
	return &Command{
		Name:  "migrate",
		Usage: usage,
		Flags: Flags(BoolFlag("dry-run")),
		Args:  Args(StringArg("direction").Default("up")),
		Run:   func(Context) error { return nil },
	}
}

func TestCommand_Usage_OverridesDerivedLine(t *testing.T) {
	root := &Command{Name: "myapp", Subcommands: []*Command{migrateCmd("myapp migrate [direction]")}}
	_, stdout, _ := exec(t, root, []string{"migrate", "--help"})
	if !strings.Contains(stdout, "\n  myapp migrate [direction]\n") {
		t.Errorf("help must print Usage verbatim as the usage line, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "[flags]") {
		t.Errorf("derived usage must not appear when Usage is set, got:\n%s", stdout)
	}
}

func TestCommand_Usage_EmptyKeepsDerivedHelp(t *testing.T) {
	withUsage := &Command{Name: "myapp", Subcommands: []*Command{migrateCmd("")}}
	_, stdout, _ := exec(t, withUsage, []string{"migrate", "--help"})
	if !strings.Contains(stdout, "\n  myapp migrate [flags] [direction]\n") {
		t.Errorf("empty Usage must keep the exact derived line, got:\n%s", stdout)
	}
}

func TestCommand_Alias_Dispatch(t *testing.T) {
	ran := false
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{
				Name:    "migrate",
				Aliases: []string{"migrations", "m"},
				Run:     func(Context) error { ran = true; return nil },
			},
		},
	}
	if code, _, stderr := exec(t, root, []string{"migrations"}); code != 0 || !ran {
		t.Fatalf("alias invocation: code=%d ran=%v stderr=%q", code, ran, stderr)
	}
	ran = false
	if code, _, _ := exec(t, root, []string{"m"}); code != 0 || !ran {
		t.Fatalf("short alias invocation failed")
	}
}

func TestCommand_Alias_CommandPathStaysCanonical(t *testing.T) {
	var path []string
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{
				Name:    "migrate",
				Aliases: []string{"migrations"},
				Run:     func(ctx Context) error { path = ctx.CommandPath(); return nil },
			},
		},
	}
	exec(t, root, []string{"migrations"})
	if len(path) != 2 || path[1] != "migrate" {
		t.Errorf("CommandPath must report canonical names, got %v", path)
	}
}

func TestCommand_Alias_ShownInParentListing(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "migrate", Aliases: []string{"migrations"}, Help: "Apply migrations", Run: func(Context) error { return nil }},
			{Name: "serve", Run: func(Context) error { return nil }},
		},
	}
	_, stdout, _ := exec(t, root, []string{"--help"})
	if !strings.Contains(stdout, "migrate, migrations") {
		t.Errorf("parent listing must show name, alias - got:\n%s", stdout)
	}
}

func TestCommand_Alias_DuplicateAgainstSiblingName(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "migrate", Aliases: []string{"status"}, Run: func(Context) error { return nil }},
			{Name: "status", Run: func(Context) error { return nil }},
		},
	}
	code, _, stderr := exec(t, root, []string{"status"})
	if code != 1 || !strings.Contains(stderr, "duplicate") {
		t.Errorf("alias colliding with sibling name must be a declaration error (exit 1), code=%d stderr=%q", code, stderr)
	}
}

func TestCommand_Alias_DuplicateAgainstSiblingAlias(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "migrate", Aliases: []string{"db"}, Run: func(Context) error { return nil }},
			{Name: "reset", Aliases: []string{"db"}, Run: func(Context) error { return nil }},
		},
	}
	code, _, stderr := exec(t, root, []string{"migrate"})
	if code != 1 || !strings.Contains(stderr, "duplicate") {
		t.Errorf("two siblings sharing an alias must be a declaration error (exit 1), code=%d stderr=%q", code, stderr)
	}
}

func TestCommand_Alias_CollidesWithInjectedCompletion(t *testing.T) {
	for _, injected := range []string{"completion", "__complete"} {
		root := &Command{
			Name: "myapp",
			Subcommands: []*Command{
				{Name: "scripts", Aliases: []string{injected}, Run: func(Context) error { return nil }},
			},
		}
		code, _, stderr := exec(t, root, []string{"scripts"})
		if code != 1 || !strings.Contains(stderr, "duplicate") {
			t.Errorf("alias %q shadowing an injected command must error (exit 1), code=%d stderr=%q", injected, code, stderr)
		}
	}
}

func TestCommand_Alias_UnknownSuggestsClosestToken(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "migrate", Aliases: []string{"migrations"}, Run: func(Context) error { return nil }},
		},
	}
	_, _, stderr := exec(t, root, []string{"migratoins"})
	if !strings.Contains(stderr, `"migrations"`) {
		t.Errorf("typo nearest an alias must suggest that alias, stderr=%q", stderr)
	}
}

func TestComplete_AliasCandidates(t *testing.T) {
	root := &Command{
		Name: "myapp",
		Subcommands: []*Command{
			{Name: "migrate", Aliases: []string{"migrations"}, Run: func(Context) error { return nil }},
		},
	}
	_, stdout, _ := exec(t, root, []string{"__complete", "--", "migr"})
	if !strings.Contains(stdout, "migrate") || !strings.Contains(stdout, "migrations") {
		t.Errorf("completion candidates must include aliases, got:\n%s", stdout)
	}
	_, stdout, _ = exec(t, root, []string{"migrations", "--help"})
	if !strings.Contains(stdout, "myapp migrate") {
		t.Errorf("help reached via alias renders the canonical usage, got:\n%s", stdout)
	}
}
