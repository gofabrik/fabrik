package shared

//fabrik:config http
type Config struct {
	Addr string `yaml:"addr" env:"DEMO_HTTP_ADDR" default:":8080"`
}
