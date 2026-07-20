package web

//fabrik:config greeter
type GreeterConfig struct {
	Kind string `yaml:"kind" env:"DEMO_GREETER_KIND" default:"hello"`
}
