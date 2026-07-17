package shared

//fabrik:config http
type Config struct {
	Addr string `yaml:"addr" env:"DEMO_HTTP_ADDR" default:":8080"`
}

//fabrik:config database
type Database struct {
	Path string `yaml:"path" env:"DEMO_DATABASE_PATH" default:"demo.db"`
}

//fabrik:config log
type Log struct {
	Level string `yaml:"level" env:"DEMO_LOG_LEVEL" default:"info"`
}

//fabrik:config crossorigin
type CrossOrigin struct {
	TrustedOrigins []string `yaml:"trusted_origins" env:"DEMO_CROSSORIGIN_TRUSTED_ORIGINS"`
}
