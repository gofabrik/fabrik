package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// ExecOption configures one [Command.Exec] invocation, with later options taking precedence.
type ExecOption func(*execConfig)

type execConfig struct {
	stdout    io.Writer
	stderr    io.Writer
	stdin     io.Reader
	parentCtx context.Context
	onError   func(Context, error) int
	env       func(string) (string, bool)
}

// WithStdout overrides [Context.Stdout], which defaults to os.Stdout.
func WithStdout(w io.Writer) ExecOption { return func(c *execConfig) { c.stdout = w } }

// WithStderr overrides [Context.Stderr] and the default error output.
func WithStderr(w io.Writer) ExecOption { return func(c *execConfig) { c.stderr = w } }

// WithStdin overrides [Context.Stdin], which defaults to os.Stdin.
func WithStdin(r io.Reader) ExecOption { return func(c *execConfig) { c.stdin = r } }

// WithSignalContext replaces the default SIGINT- and SIGTERM-aware parent context.
func WithSignalContext(ctx context.Context) ExecOption {
	return func(c *execConfig) { c.parentCtx = ctx }
}

// OnError sets the error renderer and exit-code mapper used instead of [DefaultOnError].
func OnError(fn func(Context, error) int) ExecOption {
	return func(c *execConfig) { c.onError = fn }
}

func withEnv(env func(string) (string, bool)) ExecOption {
	return func(c *execConfig) { c.env = env }
}

// Exec parses args, runs the matched handler, and returns 0 for success, 130 for cancellation, 2 for errors matching [ErrUsage], [ErrValidation], [ErrUnknownCmd], [ErrMissingArg], or [ErrMissingSubcommand], and 1 otherwise.
func (c *Command) Exec(args []string, opts ...ExecOption) int {
	cfg := execConfig{
		stdout:  os.Stdout,
		stderr:  os.Stderr,
		stdin:   os.Stdin,
		onError: DefaultOnError,
		env:     os.LookupEnv,
	}
	for _, o := range opts {
		o(&cfg)
	}

	parentCtx, cancel := cfg.signalContext()
	defer cancel()

	// Completion injection must not mutate command trees shared by concurrent calls.
	root := withInjectedCompletion(c)

	res, known, parseErr := parseArgs(root, args, cfg.env)
	ctx := &cmdContext{
		Context:    parentCtx,
		stdout:     cfg.stdout,
		stderr:     cfg.stderr,
		stdin:      cfg.stdin,
		pathCmds:   res.path,
		knownFlags: known,
	}

	if parseErr != nil {
		return cfg.onError(ctx, parseErr)
	}

	if res.help {
		renderHelp(ctx.stdout, res.path, known)
		return 0
	}
	if res.version {
		fmt.Fprintln(ctx.stdout, res.cmd.Version)
		return 0
	}

	ctx.Context = ctxWithValues(ctx.Context, res.values)

	handler := composeHandler(res)
	if err := handler(ctx); err != nil {
		return cfg.onError(ctx, err)
	}
	return 0
}

// Parse returns the resolved invocation without executing a handler and preserves Exec's error sentinels.
func (c *Command) Parse(args []string) (*ParseResult, error) {
	res, _, err := parseArgs(c, args, os.LookupEnv)
	return res, err
}

// Command returns the resolved command (the deepest matched).
func (r *ParseResult) Command() *Command { return r.cmd }

// Help reports whether --help or -h appeared on the path.
func (r *ParseResult) Help() bool { return r.help }

// Version reports whether --version was requested for a command with a Version.
func (r *ParseResult) Version() bool { return r.version }

// Context returns parsed values with process streams for accessors such as flag.Get.
func (r *ParseResult) Context() Context {
	return &cmdContext{
		Context:  ctxWithValues(context.Background(), r.values),
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		stdin:    os.Stdin,
		pathCmds: r.path,
	}
}

// composeHandler applies inherited middleware with the root outermost.
func composeHandler(res *ParseResult) Handler {
	if res.cmd.Run == nil {
		return func(ctx Context) error {
			return fmt.Errorf("%s: no Run handler defined: %w", res.cmd.Name, ErrUsage)
		}
	}
	handler := Handler(res.cmd.Run)
	// Injected completion commands run outside application middleware.
	if res.cmd.injected {
		return handler
	}
	// Walk path inside-out so the root middleware is outermost.
	for i := len(res.path) - 1; i >= 0; i-- {
		ms := res.path[i].Use
		for j := len(ms) - 1; j >= 0; j-- {
			handler = ms[j](handler)
		}
	}
	return handler
}

func (cfg *execConfig) signalContext() (context.Context, context.CancelFunc) {
	if cfg.parentCtx != nil {
		return context.WithCancel(cfg.parentCtx)
	}
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// DefaultOnError implements the rendering and exit-code contract documented by [Command.Exec].
func DefaultOnError(ctx Context, err error) int {
	if err == nil {
		return 0
	}
	prog := programName(ctx)
	stderr := ctx.Stderr()

	if errors.Is(err, context.Canceled) {
		return 130
	}

	// Missing subcommands render help without a separate error line.
	if errors.Is(err, ErrMissingSubcommand) {
		if cc, ok := ctx.(*cmdContext); ok && len(cc.pathCmds) > 0 {
			renderHelp(stderr, cc.pathCmds, cc.knownFlags)
		}
		return 2
	}

	switch {
	case errors.Is(err, ErrUsage), errors.Is(err, ErrValidation),
		errors.Is(err, ErrUnknownCmd), errors.Is(err, ErrMissingArg):
		fmt.Fprintf(stderr, "%s: %s\n", prog, displayMessage(err))
		if cc, ok := ctx.(*cmdContext); ok && len(cc.pathCmds) > 0 {
			fmt.Fprintln(stderr)
			renderHelp(stderr, cc.pathCmds, cc.knownFlags)
		}
		return 2
	default:
		fmt.Fprintf(stderr, "%s: %s\n", prog, displayMessage(err))
		return 1
	}
}

// displayMessage strips a trailing sentinel label from user-facing error text.
func displayMessage(err error) string {
	msg := err.Error()
	for _, s := range []error{
		ErrUsage, ErrValidation, ErrUnknownCmd,
		ErrMissingArg, ErrMissingSubcommand,
	} {
		suffix := ": " + s.Error()
		if strings.HasSuffix(msg, suffix) {
			return msg[:len(msg)-len(suffix)]
		}
	}
	return msg
}

func programName(ctx Context) string {
	path := ctx.CommandPath()
	if len(path) == 0 {
		return "cli"
	}
	return path[0]
}
