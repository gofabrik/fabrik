package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/config"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type storeSection struct {
	Kind string `yaml:"kind" default:"memory"`
}

func TestSection(t *testing.T) {
	path := writeYAML(t, `
http:
  addr: ":9090"
store:
  kind: sqlite
`)
	got, err := config.Load[storeSection](config.File(path), config.Section("store"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "sqlite" {
		t.Errorf("Kind = %q, want sqlite", got.Kind)
	}
}

func TestSectionMissingUsesDefaults(t *testing.T) {
	path := writeYAML(t, "http:\n  addr: \":9090\"\n")
	got, err := config.Load[storeSection](config.File(path), config.Section("store"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "memory" {
		t.Errorf("Kind = %q, want default memory", got.Kind)
	}
}

func TestSectionRejectsUnknownKeysWithin(t *testing.T) {
	path := writeYAML(t, "store:\n  kind: sqlite\n  typo: x\n")
	if _, err := config.Load[storeSection](config.File(path), config.Section("store")); err == nil {
		t.Error("unknown key inside the section must error")
	}
}

func TestKnownSectionsRejectsTypo(t *testing.T) {
	path := writeYAML(t, "stroe:\n  kind: sqlite\n")
	_, err := config.Load[storeSection](config.File(path),
		config.Section("store"), config.KnownSections("store", "http"))
	if err == nil || !strings.Contains(err.Error(), `unknown section "stroe"`) {
		t.Errorf("want unknown-section error, got %v", err)
	}
}

func TestKnownSectionsAcceptsKnown(t *testing.T) {
	path := writeYAML(t, "store:\n  kind: sqlite\nhttp:\n  addr: \":1\"\n")
	got, err := config.Load[storeSection](config.File(path),
		config.Section("store"), config.KnownSections("store", "http"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "sqlite" {
		t.Errorf("Kind = %q", got.Kind)
	}
}

type withOptionalGroup struct {
	Name string `yaml:"name"`
	TLS  *struct {
		Cert string `yaml:"cert" default:"x.pem"`
	} `yaml:"tls"`
}

func TestOptionalGroupWithTagsRejected(t *testing.T) {
	_, err := config.Load[withOptionalGroup]()
	if err == nil || !strings.Contains(err.Error(), "optional struct groups") {
		t.Errorf("want optional-group rejection, got %v", err)
	}
}

func TestPointerDurationDefault(t *testing.T) {
	type Config struct {
		Timeout *config.Duration `yaml:"timeout" default:"5s"`
	}
	cfg, err := config.Load[Config]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Timeout == nil || cfg.Timeout.Duration() != 5*time.Second {
		t.Fatalf("Timeout = %v, want 5s", cfg.Timeout)
	}
}

type EmbedFlat struct {
	Addr string `yaml:"addr" default:":8080"`
}

type EmbedNested struct {
	Addr string `yaml:"addr" default:":8080"`
}

func TestEmbeddedStructIsNestedKey(t *testing.T) {
	type Config struct {
		EmbedNested `yaml:"base"`
	}
	path := writeYAML(t, "base:\n  addr: \":9090\"\n")
	cfg, err := config.Load[Config](config.File(path))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9090" {
		t.Fatalf("Addr = %q, want :9090 (file must reach embedded fields)", cfg.Addr)
	}
	cfg, err = config.Load[Config]()
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Fatalf("Addr default = %q, want :8080", cfg.Addr)
	}
}

func TestEmbeddedStructInline(t *testing.T) {
	type Config struct {
		EmbedFlat `yaml:",inline"`
	}
	path := writeYAML(t, "addr: \":9090\"\n")
	cfg, err := config.Load[Config](config.File(path))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9090" {
		t.Fatalf("Addr = %q, want :9090 (inline flattens to parent keys)", cfg.Addr)
	}
}

func TestHiddenFieldDefaultAndEnv(t *testing.T) {
	type Config struct {
		APIKey string `yaml:"-" env:"TEST_HIDDEN_API_KEY" default:"fallback"`
	}
	cfg, err := config.Load[Config]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "fallback" {
		t.Fatalf("APIKey = %q, want default fallback", cfg.APIKey)
	}
	t.Setenv("TEST_HIDDEN_API_KEY", "from-env")
	cfg, err = config.Load[Config]()
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.APIKey != "from-env" {
		t.Fatalf("APIKey = %q, want env override", cfg.APIKey)
	}
	if dump := config.Dump(cfg); !strings.Contains(dump, "apikey: from-env") {
		t.Fatalf("Dump = %q, want apikey line", dump)
	}
}

func TestHiddenFieldNotFileSettable(t *testing.T) {
	type Config struct {
		APIKey string `yaml:"-"`
		Addr   string `yaml:"addr"`
	}
	path := writeYAML(t, "apikey: leak\naddr: \":8080\"\n")
	if _, err := config.Load[Config](config.File(path)); err == nil {
		t.Fatal("want unknown-field error for yaml:\"-\" key in file")
	}
}

func TestBytesLayer(t *testing.T) {
	type Config struct {
		Addr string `yaml:"addr" default:":8080"`
		Name string `yaml:"name"`
	}
	cfg, err := config.Load[Config](
		config.Bytes("defaults", []byte("name: base\naddr: \":9090\"\n")),
		config.Bytes("override", []byte("name: top\n")),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9090" || cfg.Name != "top" {
		t.Fatalf("cfg = %+v, want addr :9090 (from base) and name top (from override)", cfg)
	}
	_, err = config.Load[Config](config.Bytes("inline-doc", []byte("bogus: true\n")))
	if err == nil || !strings.Contains(err.Error(), "inline-doc") {
		t.Fatalf("err = %v, want parse error naming inline-doc", err)
	}
}

func TestScalarTrimming(t *testing.T) {
	type Config struct {
		Port    int             `yaml:"port" env:"TEST_TRIM_PORT"`
		Ratio   float64         `yaml:"ratio" env:"TEST_TRIM_RATIO"`
		Timeout config.Duration `yaml:"timeout" env:"TEST_TRIM_TIMEOUT"`
		Motto   string          `yaml:"motto" env:"TEST_TRIM_MOTTO"`
	}
	t.Setenv("TEST_TRIM_PORT", "8080\n")
	t.Setenv("TEST_TRIM_RATIO", " 0.5 ")
	t.Setenv("TEST_TRIM_TIMEOUT", "30s\n")
	t.Setenv("TEST_TRIM_MOTTO", " verbatim \n")
	cfg, err := config.Load[Config]()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 || cfg.Ratio != 0.5 || cfg.Timeout.Duration() != 30*time.Second {
		t.Fatalf("cfg = %+v, want trimmed scalars parsed", cfg)
	}
	if cfg.Motto != " verbatim \n" {
		t.Fatalf("Motto = %q, want string preserved verbatim", cfg.Motto)
	}
}

func TestLayerMergeSemantics(t *testing.T) {
	type Config struct {
		Server struct {
			Addr string `yaml:"addr"`
			Name string `yaml:"name"`
		} `yaml:"server"`
		Tags []string `yaml:"tags"`
	}
	cfg, err := config.Load[Config](
		config.Bytes("base", []byte("server:\n  addr: \":8080\"\n  name: base\ntags: [a, b]\n")),
		config.Bytes("overlay", []byte("server:\n  name: top\ntags: [c]\n")),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":8080" || cfg.Server.Name != "top" {
		t.Fatalf("server = %+v, want addr kept from base and name overridden", cfg.Server)
	}
	if len(cfg.Tags) != 1 || cfg.Tags[0] != "c" {
		t.Fatalf("tags = %v, want wholesale replacement [c]", cfg.Tags)
	}
}
