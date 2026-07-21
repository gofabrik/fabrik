package cli

import (
	"fmt"
	"slices"
	"strings"
)

// withInjectedCompletion clones the root before appending missing built-ins so Exec never mutates the caller's tree.
func withInjectedCompletion(c *Command) *Command {
	if c == nil {
		return nil
	}
	root := *c
	root.Subcommands = slices.Clone(c.Subcommands)
	if !hasSub(&root, "completion") {
		root.Subcommands = append(root.Subcommands, completionCmd(&root))
	}
	if !hasSub(&root, "__complete") {
		root.Subcommands = append(root.Subcommands, completeCmd(&root))
	}
	return &root
}

func hasSub(c *Command, name string) bool {
	for _, s := range c.Subcommands {
		if s != nil && s.Name == name {
			return true
		}
	}
	return false
}

// completionCmd builds the user-facing completion script command.
func completionCmd(root *Command) *Command {
	shellArg := StringArg("shell").
		Required().
		Help("bash | zsh | fish").
		OneOf("bash", "zsh", "fish")
	return &Command{
		Name: "completion",
		Help: "print a shell completion script",
		Long: "Emit a shell completion script for the given shell. " +
			"Source the output from your shell's rc file, or save it " +
			"to the appropriate per-shell completion directory.",
		Hidden:   true,
		injected: true,
		Args:     Args(shellArg),
		Run: func(ctx Context) error {
			var script string
			switch s := shellArg.Get(ctx); s {
			case "bash":
				script = bashCompletion(root.Name)
			case "zsh":
				script = zshCompletion(root.Name)
			case "fish":
				script = fishCompletion(root.Name)
			default:
				// OneOf and this switch must accept the same shell set.
				return fmt.Errorf("internal: no emitter wired up for shell %q", s)
			}
			_, err := fmt.Fprint(ctx.Stdout(), script)
			return err
		},
	}
}

// completeCmd builds the hidden runtime command invoked by generated scripts.
func completeCmd(root *Command) *Command {
	words := StringSliceArg("words").Variadic()
	return &Command{
		Name:     "__complete",
		Hidden:   true,
		injected: true,
		Args:     Args(words),
		Run: func(ctx Context) error {
			return runComplete(ctx, root, words.Get(ctx))
		},
	}
}

// pathWalk records parser state before the token being completed.
type pathWalk struct {
	// cmd is the deepest resolved command.
	cmd *Command
	// known contains every flag visible to cmd.
	known []AnyFlag
	// positionalCount identifies the next argument slot or trailing variadic.
	positionalCount int
	// pendingFlag identifies a non-boolean flag awaiting the current word as its value.
	pendingFlag AnyFlag
	// positionalsStarted prevents subcommand suggestions after the first positional.
	positionalsStarted bool
	// positionalLocked prevents flag suggestions after a variadic begins consuming tokens.
	positionalLocked bool
}

// walkPath mirrors parser traversal without parsing flag values.
func walkPath(root *Command, tokens []string) pathWalk {
	w := pathWalk{
		cmd:   root,
		known: append([]AnyFlag(nil), root.Flags...),
	}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		// The separator does not occupy a positional slot.
		if tok == "--" {
			w.positionalsStarted = true
			w.positionalLocked = true
			continue
		}
		if !w.positionalLocked && isFlagToken(tok) {
			if !strings.Contains(tok, "=") {
				if f := lookupFlagToken(w.known, tok); f != nil && !f.flagIsBool() {
					if i+1 < len(tokens) {
						i++ // consume the flag's value
						continue
					}
					w.pendingFlag = f
					continue
				}
			}
			continue
		}
		if !w.positionalsStarted {
			if sub := findSub(w.cmd, tok); sub != nil {
				w.cmd = sub
				w.known = append(w.known, sub.Flags...)
				continue
			}
		}
		w.positionalsStarted = true
		if commandHasVariadic(w.cmd) {
			w.positionalLocked = true
		}
		w.positionalCount++
	}
	return w
}

// runComplete emits prefix-matching completion candidates to ctx.Stdout, one per line.
func runComplete(ctx Context, root *Command, partial []string) error {
	cur := ""
	rest := partial
	if len(partial) > 0 {
		cur = partial[len(partial)-1]
		rest = partial[:len(partial)-1]
	}
	w := walkPath(root, rest)

	var writeErr error
	emit := func(s string) {
		if writeErr != nil {
			return
		}
		_, writeErr = fmt.Fprintln(ctx.Stdout(), s)
	}
	emitCandidates := func(fn CompleteFn, valPrefix, displayPrefix string) {
		if fn == nil {
			return
		}
		for _, c := range fn(CompleteContext{Word: valPrefix, Args: partial}) {
			if strings.HasPrefix(c, valPrefix) {
				emit(displayPrefix + c)
			}
		}
	}

	if !w.positionalLocked {
		if strings.HasPrefix(cur, "--") && strings.Contains(cur, "=") {
			eq := strings.Index(cur, "=")
			name := strings.TrimPrefix(cur[:eq], "--")
			valPrefix := cur[eq+1:]
			if f := findFlagLong(w.known, name); f != nil {
				if f.flagIsBool() {
					for _, c := range []string{"true", "false"} {
						if strings.HasPrefix(c, valPrefix) {
							emit("--" + name + "=" + c)
						}
					}
					return writeErr
				}
				emitCandidates(f.flagCompleter(), valPrefix, "--"+name+"=")
				return writeErr
			}
		}

		if w.pendingFlag != nil {
			emitCandidates(w.pendingFlag.flagCompleter(), cur, "")
			return writeErr
		}

		if strings.HasPrefix(cur, "--") {
			prefix := strings.TrimPrefix(cur, "--")
			for _, f := range w.known {
				if f.flagHidden() {
					continue
				}
				if strings.HasPrefix(f.flagName(), prefix) {
					emit("--" + f.flagName())
				}
			}
			return writeErr
		}
		if cur == "-" {
			for _, f := range w.known {
				if f.flagHidden() {
					continue
				}
				if f.flagShort() != 0 {
					emit("-" + string(f.flagShort()))
				}
				emit("--" + f.flagName())
			}
			return writeErr
		}
	}

	// The parser cannot descend into subcommands after a positional.
	if !w.positionalsStarted {
		for _, s := range w.cmd.Subcommands {
			if s.Hidden {
				continue
			}
			if strings.HasPrefix(s.Name, cur) {
				emit(s.Name)
			}
			for _, a := range s.Aliases {
				if strings.HasPrefix(a, cur) {
					emit(a)
				}
			}
		}
	}

	switch {
	case w.positionalCount < len(w.cmd.Args):
		emitCandidates(w.cmd.Args[w.positionalCount].argCompleter(), cur, "")
	case len(w.cmd.Args) > 0:
		last := w.cmd.Args[len(w.cmd.Args)-1]
		if last.argVariadic() {
			emitCandidates(last.argCompleter(), cur, "")
		}
	}
	return writeErr
}

// lookupFlagToken resolves long, short, and inline-value forms during completion traversal.
func lookupFlagToken(flags []AnyFlag, tok string) AnyFlag {
	body := strings.TrimLeft(tok, "-")
	if i := strings.Index(body, "="); i >= 0 {
		body = body[:i]
	}
	if strings.HasPrefix(tok, "--") {
		return findFlagLong(flags, body)
	}
	if len(body) == 1 {
		return findFlagShort(flags, []rune(body)[0])
	}
	return nil
}

// bashCompletion returns a script that binds completion to prog through __complete.
func bashCompletion(prog string) string {
	return `# bash completion for ` + prog + `
_` + prog + `_complete() {
    local cur words
    cur="${COMP_WORDS[COMP_CWORD]}"
    words=("${COMP_WORDS[@]:1:COMP_CWORD}")
    local IFS=$'\n'
    local out
    out=$(` + prog + ` __complete -- "${words[@]}" 2>/dev/null)
    COMPREPLY=( $(compgen -W "$out" -- "$cur") )
    return 0
}
complete -F _` + prog + `_complete ` + prog + `
`
}

func zshCompletion(prog string) string {
	return `#compdef ` + prog + `
_` + prog + `() {
    # NOTE: do NOT 'local words'; it shadows the special ` + "`words`" + `
    # variable the completion machinery exposes from the calling
    # scope, leaving the slice empty. Use a differently-named local
    # to capture the slice instead.
    local -a candidates cwords
    cwords=("${(@)words[2,$CURRENT]}")
    candidates=("${(@f)$(` + prog + ` __complete -- "${cwords[@]}" 2>/dev/null)}")
    if [[ ${#candidates[@]} -gt 0 ]]; then
        _describe 'values' candidates
    fi
}
compdef _` + prog + ` ` + prog + `
`
}

func fishCompletion(prog string) string {
	return `# fish completion for ` + prog + `
function __` + prog + `_complete
    set -l words (commandline -opc) (commandline -ct)
    set -e words[1]
    ` + prog + ` __complete -- $words 2>/dev/null
end
complete -c ` + prog + ` -f -a '(__` + prog + `_complete)'
`
}
