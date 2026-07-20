package query

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// All runs query and scans every row into a struct T.
//
// Args are normalized before they reach the driver.
func All[T any](ctx context.Context, db Executor, query string, args ...any) ([]T, error) {
	typ, err := rowType[T]()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, normArgs(args)...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close() //nolint:errcheck // read errors are reported by rows.Err; Close is cleanup

	plan, err := newScanPlan(typ, rows)
	if err != nil {
		return nil, classify(err)
	}
	out := []T{}
	for rows.Next() {
		var item T
		if err := plan.scan(rows, &item); err != nil {
			return nil, classify(err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// One runs query and scans the first row into a struct T.
//
// It returns [ErrNotFound] when the query produces no rows. Express
// single-row intent in SQL, usually with LIMIT 1.
func One[T any](ctx context.Context, db Executor, query string, args ...any) (T, error) {
	var zero T
	typ, err := rowType[T]()
	if err != nil {
		return zero, err
	}
	rows, err := db.QueryContext(ctx, query, normArgs(args)...)
	if err != nil {
		return zero, classify(err)
	}
	defer rows.Close() //nolint:errcheck // read errors are reported by rows.Err; Close is cleanup

	plan, err := newScanPlan(typ, rows)
	if err != nil {
		return zero, classify(err)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, classify(err)
		}
		return zero, ErrNotFound
	}
	var out T
	if err := plan.scan(rows, &out); err != nil {
		return zero, classify(err)
	}
	return out, nil
}

// checkRowType rejects invalid read target types before querying.
func checkRowType[T any]() error {
	_, err := rowType[T]()
	return err
}

// rowType resolves and validates T for reads.
func rowType[T any]() (reflect.Type, error) {
	var zero T
	typ := reflect.TypeOf(zero)
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("query: T must be a struct, got %v", reflect.TypeOf(&zero).Elem())
	}
	fm, err := getFieldMap(typ)
	if err != nil {
		return nil, err
	}
	for _, f := range fm.fields {
		if f.colStruct && !f.readOK {
			return nil, fmt.Errorf(
				"%w: type %s field %s (%s) implements driver.Valuer but not sql.Scanner - reads scan into the field, so the type needs a Scan method (or skip it with `db:\"-\"`)",
				ErrUnsupportedFieldType, typ, typ.Field(f.index).Name, typ.Field(f.index).Type)
		}
	}
	return typ, nil
}

// checkWritable rejects column types database/sql cannot bind.
func checkWritable(fn string, fm *fieldMap, typ reflect.Type) error {
	for _, f := range fm.fields {
		if f.colStruct && !f.writeOK {
			return fmt.Errorf(
				"%w: %s: type %s field %s (%s): the field value does not implement driver.Valuer - writes bind the field value, so Value must be in the value type's method set (a pointer-receiver Value does not qualify for a value field); add it, take the field by pointer, or skip it with `db:\"-\"`",
				ErrUnsupportedFieldType, fn, typ, typ.Field(f.index).Name, typ.Field(f.index).Type)
		}
	}
	return nil
}

// scanPlan maps result columns to struct fields once per result set.
type scanPlan struct {
	fields []int
}

// newScanPlan rejects duplicate result column names before scanning.
func newScanPlan(typ reflect.Type, rows *sql.Rows) (*scanPlan, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	seen := map[string]int{}
	for i, c := range cols {
		lc := strings.ToLower(c)
		if prev, dup := seen[lc]; dup {
			return nil, fmt.Errorf(
				"query: scan into %s: column %q appears at positions %d and %d in the result; "+
					"alias one with `AS new_name` and add a matching field "+
					"(or `db:\"new_name\"` tag) on the struct",
				typeLabel(typ), c, prev, i)
		}
		seen[lc] = i
	}

	fm, err := getFieldMap(typ)
	if err != nil {
		return nil, err
	}
	fields := make([]int, len(cols))
	for i, col := range cols {
		idx, ok := fm.colToField[strings.ToLower(col)]
		if !ok {
			fields[i] = -1
			continue
		}
		fields[i] = idx
	}
	return &scanPlan{fields: fields}, nil
}

// scan reads the current row into *dst following the plan.
func (p *scanPlan) scan(rows *sql.Rows, dst any) error {
	val := reflect.ValueOf(dst).Elem()
	targets := make([]any, len(p.fields))
	for i, idx := range p.fields {
		if idx < 0 {
			var discard any
			targets[i] = &discard
			continue
		}
		fv := val.Field(idx)
		switch fv.Type() {
		case timeType:
			targets[i] = &timeScanner{dst: fv.Addr().Interface().(*time.Time)}
		case ptrTimeType:
			targets[i] = &ptrTimeScanner{dst: fv.Addr().Interface().(**time.Time)}
		default:
			targets[i] = fv.Addr().Interface()
		}
	}
	return rows.Scan(targets...)
}

// typeLabel gives named and anonymous row types readable diagnostics.
func typeLabel(t reflect.Type) string {
	if t.Name() == "" {
		return t.String()
	}
	return t.PkgPath() + "." + t.Name()
}

var (
	timeType    = reflect.TypeOf(time.Time{})
	ptrTimeType = reflect.TypeOf((*time.Time)(nil))
	valuerType  = reflect.TypeOf((*driver.Valuer)(nil)).Elem()
	scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
)

// isColumnStruct reports whether a struct type can serve as one
// database column.
func isColumnStruct(t reflect.Type) bool {
	if t == timeType {
		return true
	}
	pt := reflect.PointerTo(t)
	return t.Implements(valuerType) || pt.Implements(valuerType) ||
		t.Implements(scannerType) || pt.Implements(scannerType)
}

// argOf normalizes time values to one cross-driver text format.
func argOf(v any) any {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if t == nil {
			return nil
		}
		return argOf(*t)
	}
	return v
}

// normArgs applies argOf to each bind argument.
func normArgs(args []any) []any {
	if len(args) == 0 {
		return args
	}
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = argOf(a)
	}
	return out
}

// timeScanner adapts common driver time encodings into time.Time.
type timeScanner struct{ dst *time.Time }

func (s *timeScanner) Scan(src any) error {
	switch v := src.(type) {
	case time.Time:
		*s.dst = v
		return nil
	case string:
		return s.parse(v)
	case []byte:
		return s.parse(string(v))
	case nil:
		*s.dst = time.Time{}
		return nil
	default:
		return fmt.Errorf("query: cannot scan %T into time.Time", src)
	}
}

func (s *timeScanner) parse(raw string) error {
	if raw == "" {
		*s.dst = time.Time{}
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			*s.dst = t
			return nil
		}
	}
	return fmt.Errorf("query: time.Time scan: cannot parse %q", raw)
}

// ptrTimeScanner scans nullable time columns into *time.Time fields.
type ptrTimeScanner struct{ dst **time.Time }

func (s *ptrTimeScanner) Scan(src any) error {
	if src == nil {
		*s.dst = nil
		return nil
	}
	var t time.Time
	inner := timeScanner{dst: &t}
	if err := inner.Scan(src); err != nil {
		return err
	}
	*s.dst = &t
	return nil
}

// fieldMap caches column metadata for one struct type.
type fieldMap struct {
	fields     []fieldInfo
	colToField map[string]int
	pkIndex    int
}

type fieldInfo struct {
	index     int    // index into reflect.Value's Field()
	column    string // database column name
	colStruct bool
	readOK    bool
	writeOK   bool
}

// fieldMapEntry memoizes successful and failed field-map builds.
type fieldMapEntry struct {
	fm  *fieldMap
	err error
}

var fieldMapCache sync.Map // reflect.Type → fieldMapEntry

// getFieldMap returns the cached field map for typ.
func getFieldMap(typ reflect.Type) (*fieldMap, error) {
	if v, ok := fieldMapCache.Load(typ); ok {
		e := v.(fieldMapEntry)
		return e.fm, e.err
	}
	fm, err := buildFieldMap(typ)
	actual, _ := fieldMapCache.LoadOrStore(typ, fieldMapEntry{fm: fm, err: err})
	e := actual.(fieldMapEntry)
	return e.fm, e.err
}

func buildFieldMap(typ reflect.Type) (*fieldMap, error) {
	fm := &fieldMap{
		colToField: map[string]int{},
		pkIndex:    -1,
	}
	// Scan matching is case-insensitive, so case-only column
	// differences are ambiguous even when a database allows them.
	colOwner := map[string]string{}

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		col := f.Tag.Get("db")
		if col == "-" {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft == timeType && f.Type != timeType && f.Type != ptrTimeType {
			return nil, fmt.Errorf(
				"%w: type %s field %s (%s): time fields are supported as time.Time or *time.Time only",
				ErrUnsupportedFieldType, typeLabel(typ), f.Name, f.Type)
		}
		if ft.Kind() == reflect.Struct && !isColumnStruct(ft) {
			return nil, fmt.Errorf(
				"%w: type %s field %s (%s) is a struct that is neither "+
					"time.Time nor a database/sql Valuer/Scanner — flatten it "+
					"into scalar fields, implement driver.Valuer + sql.Scanner, "+
					"or skip it with `db:\"-\"`",
				ErrUnsupportedFieldType, typeLabel(typ), f.Name, f.Type)
		}
		if col == "" {
			col = snakeCase(f.Name)
		}
		if !validColumn(col) {
			return nil, fmt.Errorf(
				"%w: type %s field %s maps to column %q (column names are "+
					"interpolated, not parameterized, and must be single "+
					"identifiers — no schema qualification; fix the field name "+
					"or its `db:` tag)",
				ErrInvalidIdentifier, typeLabel(typ), f.Name, col)
		}
		key := strings.ToLower(unquoteIdent(col))
		if prev, exists := colOwner[key]; exists {
			return nil, fmt.Errorf(
				"%w: type %s column %q from fields %s and %s "+
					"(check snake_case derived names and `db:` tag overrides)",
				ErrDuplicateColumn, typeLabel(typ), col, prev, f.Name)
		}
		colOwner[key] = f.Name
		fi := fieldInfo{index: i, column: col}
		if ft.Kind() == reflect.Struct && ft != timeType {
			fi.colStruct = true
			fi.readOK = f.Type.Implements(scannerType) || reflect.PointerTo(f.Type).Implements(scannerType)
			fi.writeOK = f.Type.Implements(valuerType)
		}
		fm.fields = append(fm.fields, fi)
		fm.colToField[key] = i
		if key == "id" {
			fm.pkIndex = len(fm.fields) - 1
		}
	}
	return fm, nil
}

// snakeCase converts a Go field name to its default SQL column name.
// Examples: UserID → user_id, URL → url, HTTPRequest → http_request,
// IDList → id_list, alreadyLower → already_lower.
//
// Non-ASCII field names later fail identifier validation.
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
				case prev >= 'A' && prev <= 'Z' && i+1 < len(name) && isLower(name[i+1]):
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

func isLower(b byte) bool { return b >= 'a' && b <= 'z' }
