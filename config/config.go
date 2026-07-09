// Package config loads typed configuration from YAML with explicit env
// overrides, defaults, and validation.
//
//	type Config struct {
//	    Server struct {
//	        Addr string `yaml:"addr" default:":8080"`
//	    } `yaml:"server"`
//	    CSRF struct {
//	        Secret string `yaml:"secret" env:"APP_CSRF_SECRET" secret:"true"`
//	    } `yaml:"csrf"`
//	}
//
//	cfg, err := config.Load[Config](
//	    config.File("config.yaml"),
//	    config.FileOptional("config.local.yaml"),
//	)
//
// Resolution order is: `default:` tags, YAML layers, `env:` overrides,
// then `Validate() error` if implemented. Field-level problems are returned
// as a [*LoadError]; file and YAML parse failures return directly.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type fileLayer struct {
	path     string // file path or literal-layer name
	optional bool
	literal  bool
	data     []byte
}

type loader struct {
	layers   []fileLayer
	section  []string
	sections []string
}

// Option configures [Load].
type Option func(*loader)

// File adds a required YAML layer. Later layers override earlier layers.
func File(path string) Option {
	return func(l *loader) { l.layers = append(l.layers, fileLayer{path: path}) }
}

// FileOptional adds a YAML layer only when the file exists.
func FileOptional(path string) Option {
	return func(l *loader) { l.layers = append(l.layers, fileLayer{path: path, optional: true}) }
}

// Bytes adds a YAML layer from memory. The name appears in error messages.
func Bytes(name string, data []byte) Option {
	return func(l *loader) { l.layers = append(l.layers, fileLayer{path: name, literal: true, data: data}) }
}

// Section decodes the named subtree of each YAML layer. A missing section is
// an empty layer.
func Section(name string) Option {
	return func(l *loader) { l.section = append(l.section, name) }
}

// KnownSections rejects top-level keys outside the declared shared-file
// sections.
func KnownSections(names ...string) Option {
	return func(l *loader) { l.sections = append(l.sections, names...) }
}

// Validatable is implemented by configurations with rules checked after all
// sources are applied. Returning a [*LoadError] merges its problems.
type Validatable interface {
	Validate() error
}

// Load builds a *T from the configured layers. T must be a struct.
func Load[T any](opts ...Option) (*T, error) {
	var l loader
	for _, o := range opts {
		o(&l)
	}

	dst := new(T)
	rv := reflect.ValueOf(dst).Elem()
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("config: Load[T]: T must be a struct, got %s", rv.Kind())
	}

	var probs []Problem

	applyDefaults(rv, &probs)

	for _, layer := range l.layers {
		data := layer.data
		if !layer.literal {
			var err error
			if data, err = os.ReadFile(layer.path); err != nil {
				if layer.optional && errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("config: read %s: %w", layer.path, err)
			}
		}
		if len(l.sections) > 0 {
			if err := checkSections(data, l.sections); err != nil {
				return nil, fmt.Errorf("config: %s: %w", layer.path, err)
			}
		}
		if len(l.section) > 0 {
			sub, err := sectionBytes(data, l.section)
			if err != nil {
				return nil, fmt.Errorf("config: parse %s: %w", layer.path, err)
			}
			if sub == nil {
				continue
			}
			data = sub
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true) // reject unknown YAML keys
		if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config: parse %s: %w", layer.path, err)
		}
	}

	applyEnv(rv, &probs)

	if v, ok := any(dst).(Validatable); ok {
		if err := v.Validate(); err != nil {
			var le *LoadError
			if errors.As(err, &le) {
				probs = append(probs, le.Problems...)
			} else {
				probs = append(probs, Problem{Key: "config", Message: err.Error()})
			}
		}
	}

	if len(probs) > 0 {
		return nil, &LoadError{Problems: probs}
	}
	return dst, nil
}

// checkSections rejects top-level keys outside the declared set.
func checkSections(data []byte, known []string) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	top := root.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		key := top.Content[i].Value
		if !slices.Contains(known, key) {
			return fmt.Errorf("unknown section %q (known: %s)", key, strings.Join(known, ", "))
		}
	}
	return nil
}

// sectionBytes returns the subtree under section, or nil when missing.
func sectionBytes(data []byte, section []string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	node := root.Content
	if len(node) == 0 {
		return nil, nil
	}
	cur := node[0]
	for _, name := range section {
		if cur.Kind != yaml.MappingNode {
			return nil, nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Value == name {
				next = cur.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil, nil
		}
		cur = next
	}
	return yaml.Marshal(cur)
}
