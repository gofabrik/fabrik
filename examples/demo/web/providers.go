package web

import (
	"context"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
)

// Greeter builds greeting messages; greeter.kind decides which variant is
// wired at startup.
//
//fabrik:provider:select greeter.kind
type Greeter interface {
	Greet(name string) string
}

type HelloGreeter struct{}

func (*HelloGreeter) Greet(name string) string { return "Hello, " + name + "!" }

//fabrik:provider case=hello
func NewHelloGreeter() *HelloGreeter { return &HelloGreeter{} }

type GoodbyeGreeter struct{}

func (*GoodbyeGreeter) Greet(name string) string { return "Goodbye, " + name + "!" }

//fabrik:provider case=goodbye
func NewGoodbyeGreeter() *GoodbyeGreeter { return &GoodbyeGreeter{} }

//fabrik:provider
func NewRatelimitStore() (*ratelimit.MemoryStore, func()) {
	store := ratelimit.NewMemoryStore()
	ticker := time.NewTicker(time.Minute)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				store.Sweep(context.Background(), time.Now())
			case <-done:
				return
			}
		}
	}()
	return store, func() {
		ticker.Stop()
		close(done)
	}
}
