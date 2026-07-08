package web

type Greeter struct {
	Prefix string
}

func (g *Greeter) Greet(name string) string {
	return g.Prefix + ", " + name + "!"
}

//fabrik:provider
func NewGreeter() *Greeter {
	return &Greeter{Prefix: "Hello"}
}
