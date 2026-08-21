# httpserver

`httpserver` serves any `http.Handler` with graceful shutdown.

```go
srv := httpserver.New(mux, nil)
_ = srv.Run(ctx)

s := httpserver.DefaultServer()
s.Addr = ":9000"
s.ReadTimeout = 5 * time.Second
srv = httpserver.New(mux, s)
```

`Run` allows up to 30 seconds for shutdown, treats `http.ErrServerClosed` as success, and returns listener errors.

Pass an `*http.Server` to configure the address, TLS, or timeouts; a nil value listens on `:8080`. Start from `DefaultServer()` to keep `ReadHeaderTimeout` and other safe defaults.
