# httpserver

`httpserver` serves any `http.Handler` with graceful shutdown.

```go
srv := httpserver.New(mux, nil)
_ = srv.Run(ctx)

srv = httpserver.New(mux, &http.Server{Addr: ":9000", ReadTimeout: 5 * time.Second})
```

`Run` allows up to 30 seconds for shutdown, treats `http.ErrServerClosed` as success, and returns listener errors.

Pass an `*http.Server` to configure the address, TLS, or timeouts; a nil value listens on `:8080`.
