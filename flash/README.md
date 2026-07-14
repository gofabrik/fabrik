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
| `Add(ctx, kind, text)` | Appends for the next render, atomically (optimistic CAS with retry) |
| `Take(ctx)` | Returns pending messages and clears them, atomically |
| `Peek(ctx)` | Reads without consuming |
| `Clear(ctx)` | Drops pending messages unrendered |

`Add`, `Take`, and `Clear` are atomic read-modify-writes on the session
cell (optimistic CAS, retried up to the session's `MaxRetries`), committed
immediately. A call that returns nil neither lost a message nor delivered
one twice; under sustained contention a call may exhaust its retries and
return `session.ErrVersionConflict`. No message is silently dropped; the
caller decides whether to retry. `Take` and `Clear` skip the write
(and never mint a session) when nothing is pending.

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
