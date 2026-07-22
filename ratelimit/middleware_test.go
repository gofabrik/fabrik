package ratelimit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
}

func request(h http.Handler, remote string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = remote
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_DenialCarriesQuotaHeaders(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(60).WithBurst(2))
	h := ratelimit.Middleware(lim)(okHandler())

	for i := 0; i < 2; i++ {
		rec := request(h, "203.0.113.7:1234")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
		if rec.Header().Get("RateLimit-Limit") != "2" {
			t.Errorf("RateLimit-Limit = %q", rec.Header().Get("RateLimit-Limit"))
		}
	}
	rec := request(h, "203.0.113.7:1234")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit request: %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q", got)
	}
	if rec.Header().Get("RateLimit-Remaining") != "0" || rec.Header().Get("RateLimit-Reset") == "" {
		t.Errorf("quota headers missing on denial: %v", rec.Header())
	}
}

func TestMiddleware_RetryAfterRoundsUp(t *testing.T) {
	// A 1.5-second delay must be exposed as 2 seconds, never 1.
	lim := newLimiter(t, ratelimit.Limit{Rate: 2, Period: 3 * time.Second, Burst: 1})
	h := ratelimit.Middleware(lim)(okHandler())
	request(h, "203.0.113.7:1234")
	rec := request(h, "203.0.113.7:1234")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want ceil to 2", got)
	}
}

func TestMiddleware_KeysAreIndependent(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(60).WithBurst(1))
	h := ratelimit.Middleware(lim)(okHandler())
	if rec := request(h, "203.0.113.7:1"); rec.Code != http.StatusOK {
		t.Fatal("first client denied")
	}
	if rec := request(h, "198.51.100.9:2"); rec.Code != http.StatusOK {
		t.Fatal("second client must have its own bucket")
	}
	if rec := request(h, "203.0.113.7:3"); rec.Code != http.StatusTooManyRequests {
		t.Fatal("first client's second request must deny (port must not split the key)")
	}
}

func TestMiddleware_LimitedHandlerOverride(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(60).WithBurst(1))
	h := ratelimit.Middleware(lim, ratelimit.WithLimitedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte(`{"error":"slow down"}`))
	})))(okHandler())
	request(h, "203.0.113.7:1")
	rec := request(h, "203.0.113.7:1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("denial status = %d; the limited handler customizes the body, never the 429", rec.Code)
	}
	if rec.Body.String() != `{"error":"slow down"}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("custom body not used: %q %q", rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

func TestMiddleware_LimitedHandlerBodyOnly(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(60).WithBurst(1))
	h := ratelimit.Middleware(lim, ratelimit.WithLimitedHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("try later"))
	})))(okHandler())
	request(h, "203.0.113.7:1")
	rec := request(h, "203.0.113.7:1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("bare Write must not default to 200: got %d", rec.Code)
	}
	if rec.Body.String() != "try later" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestMiddleware_LimitedHandlerWritesNothing(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(60).WithBurst(1))
	h := ratelimit.Middleware(lim, ratelimit.WithLimitedHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))(okHandler())
	request(h, "203.0.113.7:1")
	if rec := request(h, "203.0.113.7:1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("silent handler must still answer 429: got %d", rec.Code)
	}
}

func TestMiddleware_EmptyKeyPolicy(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(60).WithBurst(1))
	emptyKey := ratelimit.WithKeyFunc(func(*http.Request) string { return "" })

	open := ratelimit.Middleware(lim, emptyKey)(okHandler())
	for i := 0; i < 3; i++ {
		if rec := request(open, "203.0.113.7:1"); rec.Code != http.StatusOK {
			t.Fatal("fail-open must pass degraded requests")
		}
	}

	closed := ratelimit.Middleware(lim, emptyKey, ratelimit.WithFailClosed())(okHandler())
	rec := request(closed, "203.0.113.7:1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed degraded request: %d, want 503 (not a quota denial)", rec.Code)
	}
	for _, header := range []string{"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset", "Retry-After"} {
		if got := rec.Header().Get(header); got != "" {
			t.Errorf("degradation must not carry %s (got %q)", header, got)
		}
	}
	if rec.Body.String() != "rate limiter unavailable\n" {
		t.Errorf("degradation body = %q", rec.Body.String())
	}
}

type erroringStore struct{ ratelimit.Store }

func (erroringStore) Get(ctx context.Context, key string, now time.Time) (int64, bool, error) {
	return 0, false, errors.New("store down")
}

func TestMiddleware_StoreErrorPolicy(t *testing.T) {
	lim, err := ratelimit.New(ratelimit.PerMinute(60), erroringStore{}, ratelimit.WithClock(frozen()))
	if err != nil {
		t.Fatal(err)
	}
	open := ratelimit.Middleware(lim)(okHandler())
	if rec := request(open, "203.0.113.7:1"); rec.Code != http.StatusOK {
		t.Fatalf("fail-open on store error: %d", rec.Code)
	}
	closed := ratelimit.Middleware(lim, ratelimit.WithFailClosed())(okHandler())
	rec := request(closed, "203.0.113.7:1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed on store error: %d, want 503", rec.Code)
	}
	for _, header := range []string{"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset", "Retry-After"} {
		if got := rec.Header().Get(header); got != "" {
			t.Errorf("degradation must not carry %s (got %q)", header, got)
		}
	}
	if rec.Body.String() != "rate limiter unavailable\n" {
		t.Errorf("degradation body = %q", rec.Body.String())
	}
}

func TestKeyByIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.7:9999"
	if k := ratelimit.KeyByIP(req); k != "203.0.113.7" {
		t.Errorf("KeyByIP = %q", k)
	}
	req.RemoteAddr = "[2001:db8::1]:443"
	if k := ratelimit.KeyByIP(req); k != "2001:db8::1" {
		t.Errorf("KeyByIP v6 = %q", k)
	}
}
