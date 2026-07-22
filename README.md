# fabrik

Fabrik is a full-stack Go framework.

## Project status

Fabrik is under active development. Expect breaking changes before a stable
release.

## Install

```sh
go install github.com/gofabrik/fabrik/fabrik@latest
```

## Quick start

```sh
fabrik new hello
cd hello
fabrik run . run
```

Then, in another terminal:

```sh
curl 'localhost:8080/?name=fabrik'
# Hello, fabrik!
```

## How it works

Annotate the code with `//fabrik:*` directives:

```go
package web

//fabrik:provider
func NewGreeter() *Greeter { return &Greeter{Prefix: "Hello"} }

type Handlers struct {
	Greeter *Greeter
}

//fabrik:http GET /
func (h *Handlers) Index(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	w.Write([]byte(h.Greeter.Greet(name)))
}
```

`fabrik wire` scans these directives and generates `main.gen.go`, the app entry
point that connects directive-owned code into executable Go.

## Commands

| Command | Description |
| --- | --- |
| `fabrik new <project>` | Scaffold a new project. |
| `fabrik run` | Generate `main.gen.go`, then `go run`. |
| `fabrik build` | Generate `main.gen.go`, then `go build`. |
| `fabrik wire` | Generate `main.gen.go` from directives. |
| `fabrik wire -check` | Verify `main.gen.go` is up to date (for CI). |
| `fabrik assets require <pkg>[@version]` | Vendor a JS package (and its dependencies) into the asset tree. |
| `fabrik assets remove <pkg>`, `fabrik assets prune` | Remove a vendored package; delete orphaned files. |
| `fabrik directives` | Print the directive reference. |

Commit `main.gen.go` so plain `go build` works without fabrik installed.
Use `fabrik wire -check` in CI to verify it is current.

## Directives

See [DIRECTIVES.md](DIRECTIVES.md).

## Libraries

Fabrik is built from small Go libraries that can be used on their own or wired
together by the CLI:

- [router](router/README.md) - routing and middleware on top of `net/http`.
- [httpserver](httpserver/README.md) - serve an `http.Handler` with graceful shutdown.
- [config](config/README.md) - typed YAML configuration with defaults and env overrides.
- [templates](templates/README.md) - sectioned HTML templates with shared layouts and helpers.
- [web](web/README.md) - typed HTTP responses and request helpers.
- [assetmapper](assetmapper/README.md) - import maps, vendored browser packages, and hashed assets.
- [migrations](migrations/README.md) - forward-only SQL migrations for `database/sql`.
- [query](query/README.md) - typed reads and struct-derived writes over `database/sql`.
- [jobs](jobs/README.md) - background jobs.
- [mail](mail/README.md) - transactional email with template-rendered bodies and pluggable transports.
- [ratelimit](ratelimit/README.md) - keyed rate limiting with exact retry timing and pluggable stores.
- [session](session/README.md) - typed HTTP sessions with pluggable stores and tokens.
- [flash](flash/README.md) - one-shot messages stored in sessions.
- [validation](validation/README.md) - code-based input validation with field-keyed errors.
- [forms](forms/README.md) - HTTP request binding into typed structs, validated.
