package forms

import (
	"errors"
	"fmt"
	"mime/multipart"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/gofabrik/fabrik/validation"
)

func decode(values url.Values, files map[string][]*multipart.FileHeader, errs *validation.Errors, dst any) {
	rv := reflect.ValueOf(dst).Elem()
	if rv.Kind() != reflect.Struct {
		return
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		name, skip := fieldName(rt.Field(i))
		if skip {
			continue
		}
		if setFile(rv.Field(i), files[name]) {
			continue
		}
		submitted, present := values[name]
		if err := setField(rv.Field(i), submitted, present); err != nil {
			errs.Add(name, err.Error())
		}
	}
}

func setField(fv reflect.Value, submitted []string, present bool) error {
	// String-kind slices take every submitted value. Setting elements
	// preserves named string types.
	if fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String {
		if present {
			s := reflect.MakeSlice(fv.Type(), len(submitted), len(submitted))
			for i, v := range submitted {
				s.Index(i).SetString(v)
			}
			fv.Set(s)
		}
		return nil
	}
	// Pointer fields stay nil when absent, blank, or unsupported.
	if fv.Kind() == reflect.Pointer {
		val := first(submitted)
		if !present {
			return nil
		}
		if !isScalarKind(fv.Type().Elem().Kind()) {
			return fmt.Errorf("unsupported form field type %s", fv.Type())
		}
		if val == "" {
			return nil
		}
		elem := reflect.New(fv.Type().Elem())
		if err := setScalar(elem.Elem(), val); err != nil {
			return err
		}
		fv.Set(elem)
		return nil
	}
	if !present {
		return nil // missing fields keep zero values, including false checkboxes
	}
	return setScalar(fv, first(submitted))
}

func setScalar(fv reflect.Value, s string) error {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Bool:
		b, err := parseBool(s)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a whole number")
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a whole number")
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, fv.Type().Bits())
		if err != nil {
			return errors.New("must be a number")
		}
		fv.SetFloat(f)
	default:
		return fmt.Errorf("unsupported form field type %s", fv.Type())
	}
	return nil
}

func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1", "yes", "y":
		return true, nil
	case "off", "false", "0", "no", "n", "":
		return false, nil
	default:
		return false, errors.New("must be true or false")
	}
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// fieldName is the form field a struct field binds to, and whether the field
// is skipped entirely. Decoding and encoding both go through here, so the two
// directions cannot disagree about what a field is called.
func fieldName(ft reflect.StructField) (name string, skip bool) {
	if !ft.IsExported() {
		return "", true
	}
	name, _, _ = strings.Cut(ft.Tag.Get("form"), ",")
	if name == "-" {
		return "", true
	}
	if name == "" {
		name = snakeCase(ft.Name)
	}
	return name, false
}

// snakeCase converts a Go field name to its default form field name.
// Non-ASCII runes pass through unchanged.
func snakeCase(name string) string {
	if name == "" {
		return ""
	}
	buf := make([]byte, 0, len(name)+4)
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				prev := name[i-1]
				switch {
				case prev >= 'a' && prev <= 'z':
					buf = append(buf, '_')
				case prev >= 'A' && prev <= 'Z' && i+1 < len(name) && name[i+1] >= 'a' && name[i+1] <= 'z':
					buf = append(buf, '_')
				}
			}
			buf = append(buf, byte(r-'A'+'a'))
		default:
			buf = append(buf, string(r)...)
		}
	}
	return string(buf)
}
