# flash

One-shot session messages: added on one request, rendered by the
next, gone after that. Standalone: net/http and any mux, no
framework required.

```go
sessions, _ := session.New[Session](session.Config{...})
fl, _ := flash.New(sessions)   // any *session.Manager[T]

// the handler that acts:
fl.Add(ctx, "success", "Profile saved.")

// the handler that renders:
msgs, _ := fl.Take(ctx)        // []flash.Message{{Kind, Text}}
```

| Method | Contract |
|--------|----------|
| `Add(ctx, kind, text)` | Appends for the next render; stages into the request's session commit. Concurrent requests on one session are last-writer-wins |
| `Take(ctx)` | Returns pending messages and clears them |
| `Peek(ctx)` | Reads without consuming |
| `Clear(ctx)` | Drops pending messages unrendered |

`Message.Kind` is your rendering vocabulary: `"success"`,
`"error"`, `"info"` by convention; the package never interprets it.

Flash never learns your session type: it accepts the
`session.Registry` view of the manager and keeps its messages in a
private cell of the same session record, beside your data. In a
fabrik app the wiring is one provider:

```go
//fabrik:provider
func NewFlash(m *session.Manager[Session]) (*flash.Flash, error) {
	return flash.New(m)
}
```
