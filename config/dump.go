package config

import (
	"fmt"
	"reflect"
	"strings"
)

// Dump renders the effective configuration as "path: value" lines. Fields
// and subtrees tagged `secret:"true"` are shown as [redacted].
func Dump(cfg any) string {
	rv := reflect.ValueOf(cfg)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}

	var lines []string
	dumpStruct(rv, "", false, &lines)
	return strings.Join(lines, "\n")
}

func dumpStruct(rv reflect.Value, path string, secret bool, lines *[]string) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := yamlName(sf)
		if name == "-" {
			name = strings.ToLower(sf.Name)
		}
		fieldSecret := secret || isSecret(sf)
		p := name
		if path != "" {
			p = path + "." + name
		}

		fv := rv.Field(i)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				*lines = append(*lines, leafLine(p, "<nil>", fieldSecret))
				continue
			}
			fv = fv.Elem()
		}

		if fv.Kind() == reflect.Struct && fv.Type() != durationType {
			if fieldSecret {
				*lines = append(*lines, p+": [redacted]")
				continue
			}
			if yamlInline(sf) {
				dumpStruct(fv, path, fieldSecret, lines)
			} else {
				dumpStruct(fv, p, fieldSecret, lines)
			}
			continue
		}
		*lines = append(*lines, leafLine(p, fmt.Sprintf("%v", fv.Interface()), fieldSecret))
	}
}

func leafLine(path, val string, secret bool) string {
	if secret {
		val = "[redacted]"
	}
	return path + ": " + val
}

// isSecret treats any secret tag except "false" as enabled.
func isSecret(sf reflect.StructField) bool {
	v, ok := sf.Tag.Lookup("secret")
	return ok && v != "false"
}
