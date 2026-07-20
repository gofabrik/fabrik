// Package middleware provides reusable [cli.Middleware] implementations.
package middleware

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/gofabrik/fabrik/cli"
)

// Recover converts a panic to an error and writes its stack to ctx.Stderr.
func Recover() cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) (err error) {
			defer func() {
				if r := recover(); r != nil {
					//nolint:errcheck // Preserve the recovered panic as the handler error; stack output is best-effort.
					fmt.Fprintln(ctx.Stderr(), string(debug.Stack()))
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return next(ctx)
		}
	}
}

// Timeout cancels the handler context after d, while a non-positive duration leaves it unchanged.
func Timeout(d time.Duration) cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) error {
			if d <= 0 {
				return next(ctx)
			}
			withDeadline, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(wrapCtx(withDeadline, ctx))
		}
	}
}

// RequireEnv returns a usage error when any named environment variable is unset or empty.
func RequireEnv(names ...string) cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) error {
			for _, n := range names {
				if os.Getenv(n) == "" {
					return cli.UsageError("required environment variable %s is not set", n)
				}
			}
			return next(ctx)
		}
	}
}

// Logging writes a summary to ctx.Stderr after the handler returns in this format:
//
//	<command path> took <duration> (err: <err>)
func Logging() cli.Middleware {
	return func(next cli.Handler) cli.Handler {
		return func(ctx cli.Context) error {
			start := time.Now()
			err := next(ctx)
			path := ""
			for i, p := range ctx.CommandPath() {
				if i > 0 {
					path += " "
				}
				path += p
			}
			if err != nil {
				//nolint:errcheck // Logging must preserve the wrapped handler's error instead of replacing it with a stderr failure.
				fmt.Fprintf(ctx.Stderr(), "%s took %s (err: %v)\n", path, time.Since(start), err)
			} else {
				//nolint:errcheck // Logging has no error result to preserve when its best-effort stderr write fails.
				fmt.Fprintf(ctx.Stderr(), "%s took %s\n", path, time.Since(start))
			}
			return err
		}
	}
}

// wrapCtx replaces the embedded [context.Context] while preserving CLI streams and command path.
func wrapCtx(inner context.Context, outer cli.Context) cli.Context {
	return &derivedContext{Context: inner, parent: outer}
}

type derivedContext struct {
	context.Context
	parent cli.Context
}

func (d *derivedContext) Stdout() io.Writer     { return d.parent.Stdout() }
func (d *derivedContext) Stderr() io.Writer     { return d.parent.Stderr() }
func (d *derivedContext) Stdin() io.Reader      { return d.parent.Stdin() }
func (d *derivedContext) CommandPath() []string { return d.parent.CommandPath() }
