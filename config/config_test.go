package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/config"
)

type appConfig struct {
	Server struct {
		Addr string `yaml:"addr" default:":8080"`
		Dev  bool   `yaml:"dev"`
	} `yaml:"server"`
	Database struct {
		DSN string `yaml:"dsn" default:"demo.db"`
	} `yaml:"database"`
	Cache struct {
		StatusTTL config.Duration `yaml:"status_ttl" default:"30s"`
	} `yaml:"cache"`
	Tags []string `yaml:"tags"`
	CSRF struct {
		Secret string `yaml:"secret" env:"TEST_CSRF_SECRET" secret:"true"`
	} `yaml:"csrf"`
}

func (c appConfig) Validate() error {
	var probs []config.Problem
	if c.Server.Addr == "" {
		probs = append(probs, config.Problem{Key: "server.addr", Message: "is required"})
	}
	if len(c.CSRF.Secret) < 8 {
		probs = append(probs, config.Problem{Key: "csrf.secret", Message: "must be at least 8 characters"})
	}
	if len(probs) > 0 {
		return &config.LoadError{Problems: probs}
	}
	return nil
}

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_YAMLWithDefaults(t *testing.T) {
	path := write(t, "config.yaml", `
server:
  addr: ":9090"
  dev: true
cache:
  status_ttl: 1m
tags: [a, b]
csrf:
  secret: "longenoughsecret"
`)
	cfg, err := config.Load[appConfig](config.File(path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9090" || !cfg.Server.Dev {
		t.Errorf("server wrong: %+v", cfg.Server)
	}
	if cfg.Database.DSN != "demo.db" {
		t.Errorf("default not applied: DSN = %q", cfg.Database.DSN)
	}
	if cfg.Cache.StatusTTL.Duration() != time.Minute {
		t.Errorf("duration wrong: %v", cfg.Cache.StatusTTL)
	}
	if len(cfg.Tags) != 2 || cfg.Tags[0] != "a" {
		t.Errorf("tags wrong: %v", cfg.Tags)
	}
}

func TestLoad_DefaultDurationApplies(t *testing.T) {
	path := write(t, "config.yaml", "csrf:\n  secret: \"longenoughsecret\"\n")
	cfg, err := config.Load[appConfig](config.File(path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.StatusTTL.Duration() != 30*time.Second {
		t.Errorf("default duration = %v, want 30s", cfg.Cache.StatusTTL)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("default addr = %q", cfg.Server.Addr)
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	path := write(t, "config.yaml", `csrf:
  secret: "from-yaml-long"
`)
	t.Setenv("TEST_CSRF_SECRET", "from-env-override")
	cfg, err := config.Load[appConfig](config.File(path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CSRF.Secret != "from-env-override" {
		t.Errorf("env should win over YAML, got %q", cfg.CSRF.Secret)
	}
}

func TestLoad_LayersOverlayInOrder(t *testing.T) {
	base := write(t, "config.yaml", `
server:
  addr: ":8080"
csrf:
  secret: "longenoughsecret"
`)
	local := write(t, "config.local.yaml", "server:\n  addr: \":3000\"\n")
	cfg, err := config.Load[appConfig](config.File(base), config.FileOptional(local))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":3000" {
		t.Errorf("later layer should win, got %q", cfg.Server.Addr)
	}
}

func TestLoad_FileOptionalMissingIsFine(t *testing.T) {
	base := write(t, "config.yaml", "csrf:\n  secret: \"longenoughsecret\"\n")
	cfg, err := config.Load[appConfig](
		config.File(base),
		config.FileOptional(filepath.Join(t.TempDir(), "does-not-exist.yaml")),
	)
	if err != nil {
		t.Fatalf("missing optional file should be ignored, got %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
}

func TestLoad_RequiredFileMissingErrors(t *testing.T) {
	_, err := config.Load[appConfig](config.File(filepath.Join(t.TempDir(), "nope.yaml")))
	if err == nil {
		t.Fatal("a missing required file should error")
	}
	var le *config.LoadError
	if errors.As(err, &le) {
		t.Error("a missing file is a hard error, not an aggregated LoadError")
	}
}

func TestLoad_UnknownKeyRejected(t *testing.T) {
	path := write(t, "config.yaml", `
server:
  addr: ":8080"
  bogus: nope
csrf:
  secret: "longenoughsecret"
`)
	if _, err := config.Load[appConfig](config.File(path)); err == nil {
		t.Fatal("unknown YAML key should be rejected (KnownFields)")
	}
}

func TestLoad_BadDurationInYAML(t *testing.T) {
	path := write(t, "config.yaml", `
cache:
  status_ttl: "not-a-duration"
csrf:
  secret: "longenoughsecret"
`)
	if _, err := config.Load[appConfig](config.File(path)); err == nil {
		t.Fatal("an unparseable duration should error")
	}
}

func TestLoad_AggregatesValidationProblems(t *testing.T) {
	path := write(t, "config.yaml", `
server:
  addr: ""
csrf:
  secret: "short"
`)
	_, err := config.Load[appConfig](config.File(path))
	if err == nil {
		t.Fatal("expected validation problems")
	}
	var le *config.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("want *LoadError, got %T", err)
	}
	if len(le.Problems) != 2 {
		t.Errorf("want 2 problems, got %d: %v", len(le.Problems), le.Problems)
	}
	msg := err.Error()
	if !strings.Contains(msg, "server.addr") || !strings.Contains(msg, "csrf.secret") {
		t.Errorf("aggregated message missing keys: %s", msg)
	}
}

func TestLoad_BadEnvOverrideReported(t *testing.T) {
	type cfg struct {
		Port int `yaml:"port" env:"TEST_PORT" default:"8080"`
	}
	path := write(t, "config.yaml", "port: 9090\n")
	t.Setenv("TEST_PORT", "not-a-number")
	_, err := config.Load[cfg](config.File(path))
	var le *config.LoadError
	if !errors.As(err, &le) || len(le.Problems) != 1 || le.Problems[0].Key != "TEST_PORT" {
		t.Fatalf("want a TEST_PORT problem, got %v", err)
	}
}

func TestDump_RedactsSecrets(t *testing.T) {
	path := write(t, "config.yaml", `
server:
  addr: ":8080"
csrf:
  secret: "supersecretvalue"
`)
	cfg, err := config.Load[appConfig](config.File(path))
	if err != nil {
		t.Fatal(err)
	}
	dump := config.Dump(cfg)
	if strings.Contains(dump, "supersecretvalue") {
		t.Errorf("Dump leaked the secret:\n%s", dump)
	}
	if !strings.Contains(dump, "csrf.secret: [redacted]") {
		t.Errorf("Dump should redact csrf.secret:\n%s", dump)
	}
	if !strings.Contains(dump, "server.addr: :8080") {
		t.Errorf("Dump should show non-secret fields:\n%s", dump)
	}
}

func TestLoad_NonStructErrors(t *testing.T) {
	if _, err := config.Load[int](); err == nil {
		t.Fatal("Load[int] should error: T must be a struct")
	}
}

func TestSetScalar_AllKindsViaEnv(t *testing.T) {
	type scalars struct {
		S string          `env:"T_S"`
		B bool            `env:"T_B"`
		I int             `env:"T_I"`
		U uint16          `env:"T_U"`
		F float64         `env:"T_F"`
		L []string        `env:"T_L"`
		D config.Duration `env:"T_D"`
		P *int            `env:"T_P"`
	}
	t.Setenv("T_S", "hi")
	t.Setenv("T_B", "true")
	t.Setenv("T_I", "-7")
	t.Setenv("T_U", "42")
	t.Setenv("T_F", "3.5")
	t.Setenv("T_L", "a, b ,c")
	t.Setenv("T_D", "90s")
	t.Setenv("T_P", "5")

	cfg, err := config.Load[scalars]()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case cfg.S != "hi":
		t.Errorf("string: %q", cfg.S)
	case !cfg.B:
		t.Error("bool")
	case cfg.I != -7:
		t.Errorf("int: %d", cfg.I)
	case cfg.U != 42:
		t.Errorf("uint: %d", cfg.U)
	case cfg.F != 3.5:
		t.Errorf("float: %v", cfg.F)
	case len(cfg.L) != 3 || cfg.L[1] != "b":
		t.Errorf("[]string: %v", cfg.L)
	case cfg.D.Duration() != 90*time.Second:
		t.Errorf("duration: %v", cfg.D)
	case cfg.P == nil || *cfg.P != 5:
		t.Errorf("pointer: %v", cfg.P)
	}
}

func TestLoad_UnsupportedTypeReported(t *testing.T) {
	type bad struct {
		M map[string]string `yaml:"m" default:"x"`
	}
	_, err := config.Load[bad]()
	var le *config.LoadError
	if !errors.As(err, &le) || len(le.Problems) != 1 {
		t.Fatalf("want one problem for the unsupported default, got %v", err)
	}
	if !strings.Contains(le.Problems[0].Message, "unsupported type") {
		t.Errorf("message = %q", le.Problems[0].Message)
	}
}

func TestDuration_Helpers(t *testing.T) {
	d := config.Of(2 * time.Second)
	if d.Duration() != 2*time.Second || d.String() != "2s" {
		t.Errorf("Of/Duration/String wrong: %v", d)
	}
}

func TestDump_RedactsSecretSubtrees(t *testing.T) {
	type creds struct {
		User string `yaml:"user"`
		Pass string `yaml:"pass"`
	}
	type cfg struct {
		Public  string `yaml:"public"`
		DB      creds  `yaml:"db" secret:"true"`
		DBPtr   *creds `yaml:"dbptr" secret:"true"`
		Session struct {
			Key string `yaml:"key" secret:"true"`
		} `yaml:"session"`
	}
	c := &cfg{
		Public: "ok",
		DB:     creds{User: "admin", Pass: "hunter2"},
		DBPtr:  &creds{User: "root", Pass: "topsecret"},
	}
	c.Session.Key = "deadbeef"

	dump := config.Dump(c)
	for _, leak := range []string{"hunter2", "admin", "topsecret", "root", "deadbeef"} {
		if strings.Contains(dump, leak) {
			t.Errorf("Dump leaked %q:\n%s", leak, dump)
		}
	}
	if !strings.Contains(dump, "public: ok") {
		t.Errorf("Dump should still show public fields:\n%s", dump)
	}
	if !strings.Contains(dump, "db: [redacted]") || !strings.Contains(dump, "dbptr: [redacted]") {
		t.Errorf("secret subtrees should be redacted as a unit:\n%s", dump)
	}
	if !strings.Contains(dump, "session.key: [redacted]") {
		t.Errorf("secret leaf should be redacted:\n%s", dump)
	}
}

func TestLoad_EmptyEnvDoesNotWipe(t *testing.T) {
	path := write(t, "config.yaml", "csrf:\n  secret: \"longenoughsecret\"\n")
	t.Setenv("TEST_CSRF_SECRET", "")
	cfg, err := config.Load[appConfig](config.File(path))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CSRF.Secret != "longenoughsecret" {
		t.Errorf("empty env should not wipe the YAML value, got %q", cfg.CSRF.Secret)
	}
}

func TestLoad_AnonymousStructInlined(t *testing.T) {
	type Common struct {
		Region string `yaml:"region" default:"us-east"`
	}
	type cfg struct {
		Common `yaml:",inline"`
		Name   string `yaml:"name" default:"svc"`
	}
	path := write(t, "config.yaml", "region: eu-west\n")
	c, err := config.Load[cfg](config.File(path))
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "eu-west" || c.Name != "svc" {
		t.Errorf("inlined embedded struct wrong: %+v", c)
	}
}
