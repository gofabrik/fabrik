# ratelimit

Stdlib-only keyed GCRA rate limiting with exact retry timing and
pluggable stores.

## Limits and admission

```go
lim, err := ratelimit.New(ratelimit.PerMinute(100).WithBurst(20), ratelimit.NewMemoryStore())
if err != nil {
	return err
}

res, err := lim.Allow(ctx, clientKey)
if err != nil {
	return err
}
if !res.Allowed {
	// res.RetryAfter is the exact wait.
}
```

`Limit` permits Rate events per Period, with Burst defaulting to Rate.
`AllowN` admits up to Burst events at once. `Result.Limit` and exact
`Result.Remaining` count against burst capacity.

## Reservations

`Allow` denies work that does not fit. `Reserve` consumes the next
future slot, allowing schedulers to space work instead of retrying
together:

```go
r, err := lim.Reserve(ctx, key)
```

Schedule the absolute `ReadyAt` value directly instead of recomputing a
delay on another clock.
Reservations beyond the default 24-hour horizon return an error without
consuming capacity; `WithReservationHorizon` changes the horizon.
`lim.Wait(ctx, r)` provides cancellable in-process pacing.

## Stores

`Store` defines three atomic operations. The limiter owns the algorithm
and clock, and stores never read wall time. Each `MemoryStore` instance
limits independently, so N replicas permit up to N times the configured
rate; call `Sweep` periodically to reclaim expired entries. Use
`ratelimit/storetest` to verify custom backends.

`WithNamespace` lets several limiters share a store without sharing
buckets. `WithClock` provides a deterministic time source for tests.

## HTTP middleware

```go
limited := ratelimit.Middleware(lim)
mux.Handle("POST /login", limited(loginHandler))
```

Requests use `RemoteAddr` by default. Behind a trusted proxy, provide a
`WithKeyFunc` that reads only trusted headers. Genuine denials return
429 with quota headers and a rounded-up `Retry-After`; an empty key or
store error passes through by default, or returns a headerless 503 with
`WithFailClosed`. Session- or user-keyed limiters must handle anonymous
clients with an IP fallback or fail-closed policy.

A bare fabrik middleware declaration also wraps unmatched routes, so
declare rate limiting as named middleware and attach it per route:

```go
//fabrik:http:middleware name=ratelimit
func RateLimited(l *ratelimit.Limiter) func(http.Handler) http.Handler {
	return ratelimit.Middleware(l)
}
```
