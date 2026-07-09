package web

//fabrik:config greeter
type Config struct {
	Kind string `yaml:"kind" env:"DEMO_GREETER_KIND" default:"hello"`
}
