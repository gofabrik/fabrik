package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var durationType = reflect.TypeOf(Duration{})

// walkLeaves visits exported scalar leaves using dotted YAML paths.
func walkLeaves(rv reflect.Value, path string, fn func(fv reflect.Value, sf reflect.StructField, path string)) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := yamlName(sf)
		if name == "-" {
			// yaml:"-" hides the field from files, not defaults or env.
			name = strings.ToLower(sf.Name)
		}
		p := name
		if path != "" {
			p = path + "." + name
		}
		fv := rv.Field(i)
		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			// Match yaml.v3: embedded structs are nested unless tagged inline.
			if yamlInline(sf) {
				walkLeaves(fv, path, fn)
			} else {
				walkLeaves(fv, p, fn)
			}
			continue
		}
		fn(fv, sf, p)
	}
}

// yamlInline reports whether the field carries yaml's ",inline" option.
func yamlInline(sf reflect.StructField) bool {
	_, opts, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
	for _, o := range strings.Split(opts, ",") {
		if o == "inline" {
			return true
		}
	}
	return false
}

// yamlName returns the YAML key used by yaml.v3.
func yamlName(sf reflect.StructField) string {
	tag, ok := sf.Tag.Lookup("yaml")
	if !ok {
		return strings.ToLower(sf.Name)
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return strings.ToLower(sf.Name)
	}
	return name
}

// applyDefaults sets `default:"..."` tags before YAML layers.
func applyDefaults(rv reflect.Value, probs *[]Problem) {
	walkLeaves(rv, "", func(fv reflect.Value, sf reflect.StructField, path string) {
		// A nil *struct cannot receive nested default/env tags before YAML allocation.
		if fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.Struct && fv.Type().Elem() != durationType {
			if hasConfigTags(fv.Type().Elem(), map[reflect.Type]bool{}) {
				*probs = append(*probs, Problem{Key: path,
					Message: "optional struct groups cannot carry default: or env: tags; make the field a value struct"})
			}
			return
		}
		def, ok := sf.Tag.Lookup("default")
		if !ok {
			return
		}
		if err := setScalar(fv, def); err != nil {
			*probs = append(*probs, Problem{Key: path, Message: "invalid default: " + err.Error()})
		}
	})
}

// hasConfigTags reports whether t contains default or env tags.
func hasConfigTags(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return false
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if _, ok := sf.Tag.Lookup("default"); ok {
			return true
		}
		if _, ok := sf.Tag.Lookup("env"); ok {
			return true
		}
		if hasConfigTags(sf.Type, seen) {
			return true
		}
	}
	return false
}

// applyEnv applies opted-in environment overrides after YAML layers.
func applyEnv(rv reflect.Value, probs *[]Problem) {
	walkLeaves(rv, "", func(fv reflect.Value, sf reflect.StructField, path string) {
		tag, ok := sf.Tag.Lookup("env")
		if !ok {
			return
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			return
		}
		// Empty env values are unset; use YAML to set an empty string.
		val, ok := os.LookupEnv(name)
		if !ok || val == "" {
			return
		}
		if err := setScalar(fv, val); err != nil {
			*probs = append(*probs, Problem{Key: name, Message: err.Error()})
		}
	})
}

// setScalar parses default/env strings into supported scalar types.
func setScalar(fv reflect.Value, s string) error {
	if fv.Type() == durationType {
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return errors.New(`must be a duration like "30s" or "5m"`)
		}
		fv.Set(reflect.ValueOf(Duration{d: d}))
		return nil
	}
	if fv.Kind() == reflect.Pointer {
		elem := reflect.New(fv.Type().Elem())
		if err := setScalar(elem.Elem(), s); err != nil {
			return err
		}
		fv.Set(elem)
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			return errors.New("must be true or false")
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a whole number")
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a non-negative whole number")
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(strings.TrimSpace(s), fv.Type().Bits())
		if err != nil {
			return errors.New("must be a number")
		}
		fv.SetFloat(f)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element type %s", fv.Type().Elem())
		}
		parts := splitComma(s)
		out := reflect.MakeSlice(fv.Type(), len(parts), len(parts))
		for i, p := range parts {
			out.Index(i).SetString(p)
		}
		fv.Set(out)
	default:
		return fmt.Errorf("unsupported type %s", fv.Type())
	}
	return nil
}

// splitComma returns trimmed, non-empty comma-separated parts.
func splitComma(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
