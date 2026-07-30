# flash

One-shot messages: added on one request, rendered by the next, then
removed.

```go
fl, _ := flash.New(registry)

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

`Add`, `Take`, and `Clear` are atomic read-modify-writes on a private
state cell (optimistic CAS with the registry's retry policy), committed
immediately. A call that returns nil neither lost a message nor delivered
one twice; under sustained contention a call may exhaust its retries and
return the registry's conflict error. No message is silently dropped; the
caller decides whether to retry. `Take` and `Clear` skip the write when
nothing is pending.

`Message.Kind` is your rendering vocabulary: `"success"`,
`"error"`, `"info"` by convention; the package never interprets it.

Flash keeps its data in a private cell registered by `New`; callers
only need to retain the returned `*flash.Flash`.
