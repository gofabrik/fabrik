// Package cli provides typed commands, flags, arguments, middleware, help, and shell completion.
package cli

import (
	"context"
	"errors"
	"io"
)

var (
	// ErrUsage marks invocation errors for which the default renderer appends command help.
	ErrUsage = errors.New("usage error")

	// ErrValidation is wrapped when a [Flag] or [Arg] validator rejects a value.
	ErrValidation = errors.New("validation failed")

	// ErrUnknownCmd is wrapped when the command tree does not contain a requested subcommand.
	ErrUnknownCmd = errors.New("unknown command")

	// ErrMissingArg is wrapped when a required positional argument is absent.
	ErrMissingArg = errors.New("missing argument")

	// ErrMissingSubcommand marks a bare grouping-only command and renders help without a separate error line by default.
	ErrMissingSubcommand = errors.New("missing subcommand")
)

// Command defines either the root command or a subcommand.
type Command struct {
	// Name is the program name for the root or the invocation token for a subcommand.
	Name string

	// Help is the summary shown in command listings and --help.
	Help string

	// Long is the optional description shown below the --help usage line.
	Long string

	// Version is the string emitted by --version.
	Version string

	// Flags declares command flags inherited by descendants.
	Flags []AnyFlag

	// Args bind in order with required values first and at most one final variadic; -- allows flag-shaped positional values.
	Args []AnyArg

	// Subcommands dispatch by Name, while Run handles a bare invocation when both are present.
	Subcommands []*Command

	// Use applies inherited middleware in declaration order with Use[0] outermost.
	Use []Middleware

	// Run is invoked after parsing succeeds. It may be nil for a
	// grouping-only command; invoking such a command without a
	// subcommand prints help and exits with the usage code.
	Run func(Context) error

	// Examples are shown in the --help Examples section.
	Examples []Example

	// Hidden omits the command from parent listings.
	Hidden bool

	// injected exempts built-in completion commands from inherited flag policy and middleware.
	injected bool
}

// Example is a single entry in a command's --help examples section.
type Example struct {
	// Cmd is the example invocation line.
	Cmd string
	// Help is a one-line description rendered next to Cmd.
	Help string
}

// Context extends [context.Context] with invocation-scoped streams and command metadata.
type Context interface {
	context.Context

	// Stdout returns the writer set by [WithStdout], or os.Stdout by default.
	Stdout() io.Writer

	// Stderr returns the writer set by [WithStderr], or os.Stderr by default.
	Stderr() io.Writer

	// Stdin returns the reader set by [WithStdin], or os.Stdin by default.
	Stdin() io.Reader

	// CommandPath returns command names from the root through the current command.
	CommandPath() []string
}

// Handler is the shape of a runnable command body.
type Handler func(Context) error

// Middleware wraps a Handler in declaration order from outermost to innermost.
type Middleware func(Handler) Handler

// AnyFlag lets the parser handle typed flags uniformly and cannot be implemented outside this package.
type AnyFlag interface {
	flagName() string
	flagShort() rune
	flagHelp() string
	flagEnv() string
	flagRequired() bool
	flagHidden() bool
	flagGroup() string
	flagIsBool() bool
	flagDefaultText() string
	flagPlaceholder() string
	flagApplyString(values, string) error
	flagApplyEnv(values, func(string) (string, bool)) error
	flagValidate(values) error
	flagPresent(values) bool
	flagHasDefault() bool
	flagCompleter() CompleteFn
}

// AnyArg lets the parser handle typed arguments uniformly and cannot be implemented outside this package.
type AnyArg interface {
	argName() string
	argHelp() string
	argRequired() bool
	argVariadic() bool
	argIsSlice() bool
	argApplyString(values, string) error
	argPresent(values) bool
	argValidate(values) error
	argCompleter() CompleteFn
}

// Flags builds a []AnyFlag from typed flag values.
func Flags(flags ...AnyFlag) []AnyFlag {
	return flags
}

// Args is the positional equivalent of [Flags].
func Args(args ...AnyArg) []AnyArg {
	return args
}

// values keys invocation data by accessor identity so concurrent Exec calls do not share state.
type values map[any]any

type valuesKey struct{}

func valuesFromCtx(ctx context.Context) values {
	v, _ := ctx.Value(valuesKey{}).(values)
	if v == nil {
		return values{}
	}
	return v
}

func ctxWithValues(parent context.Context, v values) context.Context {
	return context.WithValue(parent, valuesKey{}, v)
}

// cmdContext retains the resolved path and inherited flags for error rendering.
type cmdContext struct {
	context.Context
	stdout     io.Writer
	stderr     io.Writer
	stdin      io.Reader
	pathCmds   []*Command
	knownFlags []AnyFlag
}

func (c *cmdContext) Stdout() io.Writer { return c.stdout }
func (c *cmdContext) Stderr() io.Writer { return c.stderr }
func (c *cmdContext) Stdin() io.Reader  { return c.stdin }
func (c *cmdContext) CommandPath() []string {
	out := make([]string, len(c.pathCmds))
	for i, p := range c.pathCmds {
		out[i] = p.Name
	}
	return out
}

// CompleteContext provides the current word and all tokens, with Word as the final Args element.
type CompleteContext struct {
	Word string
	Args []string
}

// CompleteFn returns candidates for the current token, and the runtime discards entries without the Word prefix.
type CompleteFn func(CompleteContext) []string

// StaticValues returns a [CompleteFn] that always suggests the same candidates.
func StaticValues(values ...string) CompleteFn {
	cp := make([]string, len(values))
	copy(cp, values)
	return func(CompleteContext) []string { return cp }
}
