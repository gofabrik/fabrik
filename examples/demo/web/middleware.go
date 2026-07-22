package web

import (
	"net/http"

	"github.com/gofabrik/fabrik/ratelimit"
)

//fabrik:http:middleware name=greetlimit
func GreetRateLimited(store *ratelimit.MemoryStore) (func(http.Handler) http.Handler, error) {
	l, err := ratelimit.New(ratelimit.PerMinute(60).WithBurst(12), store)
	if err != nil {
		return nil, err
	}
	return ratelimit.Middleware(l), nil
}
