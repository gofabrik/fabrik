package forms

// Encoding is the other direction of [decode]: a value becomes the form values
// it would have submitted. Both walk fields through [fieldName], so what a
// field is called cannot differ between reading a submission and redisplaying
// one.

import (
	"net/url"
	"reflect"
	"strconv"
)

// encode returns the values src would have submitted.
//
// A field that a browser would not send is left out rather than sent empty: a
// false bool, like an unchecked box, and a nil pointer, like an untouched
// optional field. Decoding treats an absent field as exactly those, so what
// this writes reads back the same way.
//
// Uploads are skipped. A file input cannot be given a value, so there is
// nothing to write for one.
func encode(src any) url.Values {
	rv := reflect.ValueOf(src)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return url.Values{}
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return url.Values{}
	}

	out := url.Values{}
	rt := rv.Type()
	for i := range rt.NumField() {
		name, skip := fieldName(rt.Field(i))
		if skip {
			continue
		}
		encodeField(out, name, rv.Field(i))
	}
	return out
}

func encodeField(out url.Values, name string, fv reflect.Value) {
	switch {
	case fv.Type() == fileType || fv.Type() == fileSliceType:
		return

	case fv.Kind() == reflect.Pointer:
		if fv.IsNil() {
			return
		}
		encodeField(out, name, fv.Elem())

	case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String:
		for i := range fv.Len() {
			out.Add(name, fv.Index(i).String())
		}

	default:
		if s, ok := formatScalar(fv); ok {
			out.Set(name, s)
		}
	}
}

// formatScalar renders one value the way a form would carry it. It reports
// false for a kind that is not written at all, including a false bool.
func formatScalar(fv reflect.Value) (string, bool) {
	switch fv.Kind() {
	case reflect.String:
		return fv.String(), true
	case reflect.Bool:
		// Only a checked box is submitted, so false writes nothing.
		if fv.Bool() {
			return "true", true
		}
		return "", false
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(fv.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(fv.Uint(), 10), true
	case reflect.Float32:
		return strconv.FormatFloat(fv.Float(), 'g', -1, 32), true
	case reflect.Float64:
		return strconv.FormatFloat(fv.Float(), 'g', -1, 64), true
	}
	return "", false
}
