# cli

An opinionated, typed CLI library for Go.

## Features

| Feature | Behavior |
|---|---|
| Typed flag access | `var port = cli.IntFlag("port")` then `port.Get(ctx)` returns `int`. No string keys, no reflection, no type assertions. |
| One `Command` type | The root and every subcommand use the same struct. |
| Inherited flags | A flag declared on a parent is visible from every descendant handler via the same `Get(ctx)`. |
| Flag validators | `.Required()`, `.OneOf(...)`, and `.Validate(fn)` run before the handler. |
| Repeatable + variadic | `cli.StringSliceFlag` for repeatable flags, `cli.StringSliceArg(name).Variadic()` for catch-all positionals. |
| Middleware | `Use: []cli.Middleware{...}` composes outer-to-inner and inherits from parent commands. |
| Shell completion | Auto-injected `completion bash\|zsh\|fish` scripts request candidates from the binary at runtime. |
| Generated help | `--help` derives usage, arguments, flag groups, defaults, environment bindings, and wrapping from typed definitions. |
| Standard interfaces | Uses `context.Context`, `errors.Is`, and `func(Handler) Handler`. |
| In-process tests | `root.Exec(args, cli.WithStdout(buf), cli.WithStderr(buf))` accepts explicit arguments and streams. |

## Install

```bash
go get github.com/gofabrik/fabrik/cli
```

The core depends only on the standard library and `golang.org/x/term`.

## Quickstart

```go
package main

import (
	"fmt"
	"os"

	"github.com/gofabrik/fabrik/cli"
)

// Flag variables are typed accessors.
var (
	port    = cli.IntFlag("port").Default(8080).Env("PORT").Help("port to listen on")
	verbose = cli.BoolFlag("verbose").Short('v').Help("chatty logs")
	name    = cli.StringArg("name").Required().Help("server name")
)

func main() {
	root := &cli.Command{
		Name:  "greet",
		Help:  "say hello over HTTP",
		Flags: cli.Flags(port, verbose),
		Args:  cli.Args(name),
		Run: func(ctx cli.Context) error {
			fmt.Fprintf(ctx.Stdout(), "hello, %s — listening on :%d (verbose=%v)\n",
				name.Get(ctx), port.Get(ctx), verbose.Get(ctx))
			return nil
		},
	}
	os.Exit(root.Exec(os.Args[1:]))
}
```

```
$ greet --port 9090 alice
hello, alice — listening on :9090 (verbose=false)

$ greet --help
Usage:
  greet [flags] <name>
...
```

## Flags

### Constructors

```go
cli.StringFlag(name)        // *Flag[string]
cli.IntFlag(name)           // *Flag[int]
cli.Int64Flag(name)         // *Flag[int64]
cli.FloatFlag(name)         // *Flag[float64]
cli.BoolFlag(name)          // *Flag[bool]
cli.DurationFlag(name)      // *Flag[time.Duration]
cli.TimeFlag(name)          // *Flag[time.Time]  (RFC3339)
cli.StringSliceFlag(name)   // *Flag[[]string]   (repeatable)
cli.IntSliceFlag(name)      // *Flag[[]int]
cli.CustomFlag[T](name, parse)  // user-defined type
```

### Builder methods

All return `*Flag[T]` for chaining.

| Method | Purpose |
|---|---|
| `.Default(v)` | Value `Get` returns when the flag is unset. |
| `.Env(name)` | Bind to an env var (used when the flag is not on the command line). |
| `.Short(r)` | Single-character alias (`-v`). |
| `.Help(s)` | One-line description shown in `--help`. |
| `.Required()` | Parse fails if the flag is unset and no default/env value resolves. |
| `.Validate(fn)` | Custom validator; runs after parse. |
| `.OneOf(values...)` | Restrict accepted values; also feeds shell completion. |
| `.Placeholder(s)` | Metavariable in help (`--port <N>`). Defaults to upper-cased name. |
| `.Group(name)` | Categorise the flag under a header in help. |
| `.Hidden()` | Omit from help; still parseable. |
| `.Complete(fn)` | Shell-completion strategy. |

### Accessors

```go
v := port.Get(ctx)            // parsed value or fallback
v, set := port.Lookup(ctx)    // whether a value was supplied
```

## Args

Positional arguments use the same builder shape.

```go
cli.StringArg(name)      // *Arg[string]
cli.IntArg(name)         // *Arg[int]
cli.StringSliceArg(name) // *Arg[[]string]  (combine with .Variadic())
cli.CustomArg[T](name, parse)
```

| Builder | Purpose |
|---|---|
| `.Required()` | Parse fails if the arg is missing. |
| `.Default(v)` | Used when not supplied (only valid on optional args). |
| `.Help(s)` | Description in help. |
| `.Validate(fn)` | Custom validator. |
| `.OneOf(values...)` | Restrict accepted values + completion. |
| `.Variadic()` | Slice arg captures every remaining token. Must be last. |
| `.Complete(fn)` | Shell-completion strategy. |

Rules enforced at declaration time:

- At most one variadic, and only as the last arg.
- Required args must precede optional args.
- Slice args must be `.Variadic()`.

A token starting with `-` is normally a flag. To pass `-1` (or similar) as a positional value, use `--`: `myapp cmd -- -1`. Tokens after a variadic positional are likewise positional regardless of leading dash.

## Commands

```go
type Command struct {
    Name        string         // invocation token (and program name on the root)
    Help        string         // one-line summary
    Long        string         // optional long description
    Version     string         // shown on --version (usually root only)
    Flags       []AnyFlag
    Args        []AnyArg
    Subcommands []*Command
    Use         []Middleware   // applied outer-to-inner; inherited by children
    Run         func(Context) error  // nil for grouping-only commands
    Examples    []Example
    Hidden      bool
}
```

Top-level execution:

```go
os.Exit(root.Exec(os.Args[1:]))
```

The root is a `Command`; top-level streams, signal context, and error rendering are configured with `ExecOption` values.

### Subcommands

Subcommands are `*Command` values nested under `Subcommands` and inherit parent flags and middleware.

```go
db := &cli.Command{
    Name:        "db",
    Help:        "database utilities",
    Flags:       cli.Flags(dsn),                 // visible to migrate, status, ...
    Subcommands: []*cli.Command{migrateCmd, statusCmd},
}

root := &cli.Command{
    Name:        "myapp",
    Subcommands: []*cli.Command{serveCmd, db},
}
```

Routing:

- Tokens are matched against subcommand names left-to-right; the deepest match wins.
- Flags can appear anywhere (before or after the subcommand) for normal commands. A command with a variadic positional locks flag parsing after the first positional so the variadic catches every remaining token unchanged.
- Bare invocation of a grouping-only command (`Run` nil, `Subcommands` set) renders help without an error line and exits 2.
- Unknown subcommand names suggest the closest match via Levenshtein distance.

### Middleware

```go
type Handler func(Context) error
type Middleware func(Handler) Handler
```

Declaration order is outer-to-inner: `Use[0]` is the outermost wrapper. Middleware on a parent command applies to every descendant.

The library ships a small set in [`./middleware`](./middleware): `Recover`, `Timeout`, `RequireEnv`, `Logging`.

## Help, errors, completion

Help, error rendering, and completion derive from the same typed definitions.

### Help

`--help` and `-h` are reserved on every command. The renderer:

- Detects terminal width via `golang.org/x/term` and wraps long descriptions; falls back to 80 cols when output is not a TTY.
- Bold section headers (`Usage:`, `Flags:`, etc.) when stderr/stdout is a TTY and `NO_COLOR` is unset.
- Annotates flags with `(required)`, `(default: X)`, `(env: VAR)` derived from the builder calls.
- Groups flags by `.Group()` if set.

### Errors

Errors are values. The library exports sentinels callers can match with `errors.Is`:

```go
ErrUsage              // bad flags / args
ErrValidation         // .Validate() / .OneOf() rejected a value
ErrUnknownCmd         // unknown subcommand
ErrMissingArg         // required positional missing
ErrMissingSubcommand  // grouping-only command invoked bare
```

Handler code can use `cli.UsageError(format, args...)` to make the default renderer append command help.

| Error class | Stderr output | Exit |
|---|---|---|
| `nil` | none | 0 |
| `ErrMissingSubcommand` | help only, no message line | 2 |
| `ErrUsage` / `ErrValidation` / `ErrUnknownCmd` / `ErrMissingArg` | `prog: <message>` + relevant help | 2 |
| `context.Canceled` | nothing | 130 |
| any other | `prog: <message>` | 1 |

Pass `cli.OnError(fn)` to `Exec` to override rendering; custom renderers can delegate cases to `cli.DefaultOnError`.

### Shell completion

The library auto-injects two hidden subcommands on every `Exec`:

```
$ myapp completion bash > /etc/bash_completion.d/myapp
$ myapp completion zsh  > "${fpath[1]}/_myapp"
$ myapp completion fish > ~/.config/fish/completions/myapp.fish
```

The generated scripts call back into the binary via a hidden `__complete` subcommand, which walks the command tree against the partial token list and emits candidates:

- Subcommand names at the resolved command level
- Long flag names after `--<prefix>`
- Short + long flag names after a bare `-`
- A flag's value via its `Complete(fn)` (or `OneOf(values...)`) for both `--mode <TAB>` and `--mode=<TAB>` forms
- Bool flags as `--verbose=true` / `--verbose=false` for the inline form
- The next positional's `Complete(fn)`, or the trailing variadic's once positionals are exhausted

Built-in candidate sources:

```go
cli.StaticValues(values...)  // fixed list
```

`OneOf` automatically registers a `StaticValues` completer alongside its validator.

## Configuration

```go
type ExecOption func(*execConfig)

cli.WithStdout(w io.Writer)
cli.WithStderr(w io.Writer)
cli.WithStdin(r io.Reader)
cli.WithSignalContext(ctx context.Context)   // default: SIGINT + SIGTERM aware
cli.OnError(func(Context, error) int)        // override the default renderer
```

`Exec` snapshots the command tree before injecting completion subcommands, leaving the caller's `*Command` safe for concurrent use.

## Testability

```go
func TestServe(t *testing.T) {
    var out, errOut bytes.Buffer
    root := newRoot()

    code := root.Exec([]string{"serve", "--port", "9090"},
        cli.WithStdout(&out),
        cli.WithStderr(&errOut),
    )
    if code != 0 {
        t.Fatalf("exit %d: %s", code, errOut.String())
    }
}
```

`root.Parse(args)` exposes the resolved command and values without executing the handler.

## Status

Alpha. Public API may change.
