package query

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSON wraps any Go value as a JSON-encoded column.
//
//	type Event struct {
//	    ID       int64
//	    Metadata query.JSON[map[string]any]
//	}
//
// It works with TEXT, JSON, and JSONB columns. NULL scans reset V to
// its zero value.
type JSON[T any] struct {
	V T
}

// Scan implements [database/sql.Scanner].
func (j *JSON[T]) Scan(src any) error {
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	case nil:
		var zero T
		j.V = zero
		return nil
	default:
		return fmt.Errorf("query.JSON: cannot scan %T", src)
	}
	// Reset first: json.Unmarshal merges into existing maps and structs.
	var zero T
	j.V = zero
	return json.Unmarshal(data, &j.V)
}

// Value implements [database/sql/driver.Valuer].
func (j JSON[T]) Value() (driver.Value, error) {
	b, err := json.Marshal(j.V)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
