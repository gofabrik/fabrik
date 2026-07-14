# config

Typed application configuration for Go: YAML files, explicit per-field environment overrides, defaults, and validation. One dependency: `gopkg.in/yaml.v3`.

## Why YAML-first

YAML carries structure. Environment overrides are opt-in through `env:` tags.

## Usage

`config.yaml`:

```yaml
server:
  addr: ":8080"
cache:
  status_ttl: 30s
csrf:
  secret: ""        # set per deploy by env
```

The struct uses `yaml` for structure, `env` for overrides, and `default` for fallbacks:

```go
type Config struct {
	Server struct {
		Addr string `yaml:"addr" default:":8080"`
	} `yaml:"server"`
	Cache struct {
		StatusTTL config.Duration `yaml:"status_ttl" default:"30s"`
	} `yaml:"cache"`
	CSRF struct {
		Secret string `yaml:"secret" env:"APP_CSRF_SECRET" secret:"true"`
	} `yaml:"csrf"`
}

// Optional value-shape rules, checked after all sources are applied.
func (c Config) Validate() error {
	if len(c.CSRF.Secret) < 32 {
		return &config.LoadError{Problems: []config.Problem{
			{Key: "csrf.secret", Message: "must be at least 32 characters"},
		}}
	}
	return nil
}
```

Load once at startup, then use plain typed fields:

```go
cfg, err := config.Load[Config](
	config.File("config.yaml"),                // required base
	config.FileOptional("config.local.yaml"),  // dev overlay, applied if present
)
if err != nil {
	return err
}

addr := cfg.Server.Addr
ttl := cfg.Cache.StatusTTL.Duration()
```

Layers can also come from memory:

```go
//go:embed defaults.yaml
var defaults []byte

cfg, err := config.Load[Config](
	config.Bytes("defaults", defaults),
	config.FileOptional("config.yaml"),
)
```

Override a single field in production without touching the file:

```console
$ APP_CSRF_SECRET=$(cat /run/secrets/csrf) ./app
```

## Sections

Several structs can share one file. `config.Section("store")` decodes only the keys under `store:`.

```go
type StoreConfig struct {
	Kind string `yaml:"kind" default:"memory"`
}

sc, err := config.Load[StoreConfig](
	config.FileOptional("config.yaml"),
	config.Section("store"),
)
```

## Resolution order

Resolution runs in this order: `default:` tags, YAML layers in option order, `env:` overrides, then `Validate()`.

Field-level problems from defaults, env overrides, and validation are collected into one `*LoadError`:

```
config: 2 problems:
  csrf.secret: must be at least 32 characters
  server.addr: is required
```

## Behavior

- **A later layer overrides mappings field by field, but replaces lists wholesale.** A `config.local.yaml` that sets one key under `server:` leaves the base layer's sibling keys intact; a list value replaces the base layer's entire list - there is no element-level merge.
- **An empty env value is treated as unset.** `FOO=` does not override a configured value; set an empty string in YAML.
- **Unknown YAML keys are rejected.** A `default:` or `env:` value on an unsupported scalar type is a load error.
- File-read and YAML-parse failures are returned directly, before field-level resolution.

## Field tags

| Tag | Meaning |
|---|---|
| `yaml:"name"` | Key in the file and dotted path in errors. `yaml:"-"` hides the field from the file; `default:` and `env:` still apply. |
| `default:"v"` | Value used when nothing else sets the field. |
| `env:"NAME"` | Override from `$NAME`. |
| `secret:"true"` | Redacted by `config.Dump`. |

## Types

`default:` tags and `env:` overrides parse into `string`, `bool`, sized `int`/`uint`/`float` kinds, `[]string`, pointers to scalar values, nested structs, and `config.Duration`. Non-string scalar inputs are trimmed; strings are taken verbatim.

YAML files can use anything yaml.v3 can decode into the target field. The list above is the string parser used by defaults and env.

## Dump

```go
slog.Info("config loaded", "effective", config.Dump(cfg))
```

Renders `path: value` lines with `secret:"true"` fields and subtrees shown as `[redacted]`.
