# fabrik

Fabrik is a full-stack Go framework.

## Install

```sh
go install github.com/gofabrik/fabrik/fabrik@latest
```

## Quick start

```sh
fabrik new hello
cd hello
fabrik run
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

`fabrik wire` scans these directives and generates `main.gen.go`, the composition
root that builds providers, injects them into your handler structs, and registers
routes on the [fabrik router](router/). The generated code depends only on the
standard library and the router, and the router itself is standard library only.

## Commands

| Command | Description |
| --- | --- |
| `fabrik new <project>` | Scaffold a new project. |
| `fabrik run` | Generate wiring, then `go run`. |
| `fabrik build` | Generate wiring, then `go build`. |
| `fabrik wire` | Scan directives and generate `main.gen.go`. |
| `fabrik wire -check` | Verify `main.gen.go` is up to date (for CI). |
| `fabrik directives` | Print the directive reference. |

Commit `main.gen.go` so plain `go build` works without fabrik installed.
Use `fabrik wire -check` in CI to verify it is current.

## Directives

See [DIRECTIVES.md](DIRECTIVES.md), generated from the same registry the tool runs.

- `//fabrik:provider` - mark a constructor whose return value is injected into handler
  structs and other providers by matching types.
- `//fabrik:http METHOD /path` - register a handler on the given route. The handler
  is a standard `func(http.ResponseWriter, *http.Request)`, on a plain function or a
  method of a wired struct.
