package web

import "github.com/gofabrik/fabrik/cache"

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
func NewGreetingCache(store cache.Store) (*cache.Cache[[]Greeting], error) {
	return cache.New[[]Greeting](store, cache.WithNamespace("greetings"))
}
