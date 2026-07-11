package query

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Constraint sentinels wrap raw driver errors so errors.Is works
// across supported backends.
var (
	ErrNotFound   = errors.New("query: not found")
	ErrUnique     = errors.New("query: unique constraint violation")
	ErrForeignKey = errors.New("query: foreign key violation")
	ErrCheck      = errors.New("query: check constraint violation")
)

// ErrInvalidIdentifier is returned when an interpolated table or
// column identifier is not syntactically valid.
//
// Identifiers cannot be bound as placeholders. Keep table names,
// column tags, and where fragments developer-controlled.
var ErrInvalidIdentifier = errors.New("query: invalid SQL identifier")

// ErrDuplicateColumn is returned when two exported fields resolve to
// the same column name.
var ErrDuplicateColumn = errors.New("query: duplicate column")

// ErrUnsupportedFieldType is returned when a struct field cannot be
// used as a single database column.
//
// Plain nested structs are not flattened. Use scalar fields,
// time.Time, *time.Time, [JSON], or a type with the required
// database/sql Valuer or Scanner method.
var ErrUnsupportedFieldType = errors.New("query: unsupported struct field type")

// ErrNoColumns is returned by write helpers when the row type
// contributes no columns.
//
// [Insert] uses INSERT DEFAULT VALUES for a single zero auto-PK row.
// [InsertMany] has no portable multi-row equivalent.
var ErrNoColumns = errors.New("query: struct has no columns to write")

// Classifier inspects a raw driver error and returns one of the
// sentinel errors above (ErrUnique, ErrForeignKey, ErrCheck) when
// the error matches. Returns nil to defer to other classifiers.
type Classifier func(err error) (sentinel error)

var (
	classifiersMu sync.RWMutex
	classifiers   []Classifier
)

// RegisterClassifier registers a driver-specific classifier.
// Registered classifiers run before the built-ins, in registration
// order. Returning nil defers to the next classifier.
func RegisterClassifier(c Classifier) {
	if c == nil {
		panic("query: RegisterClassifier called with nil Classifier")
	}
	classifiersMu.Lock()
	defer classifiersMu.Unlock()
	classifiers = append(classifiers, c)
}

// classify wraps driver errors with the first matching sentinel.
func classify(err error) error {
	if err == nil {
		return nil
	}
	classifiersMu.RLock()
	cs := append([]Classifier(nil), classifiers...)
	classifiersMu.RUnlock()
	for _, c := range cs {
		if sentinel := c(err); sentinel != nil {
			return fmt.Errorf("%w: %w", sentinel, err)
		}
	}
	// Built-ins run after registered classifiers regardless of init order.
	for _, c := range []Classifier{classifyPostgres, classifySQLite} {
		if sentinel := c(err); sentinel != nil {
			return fmt.Errorf("%w: %w", sentinel, err)
		}
	}
	return err
}

// sqlStater is the constraint-code API shared by pgx and lib/pq.
type sqlStater interface {
	SQLState() string
}

// classifyPostgres maps constraint SQLSTATEs to sentinels.
func classifyPostgres(err error) error {
	var st sqlStater
	if !errors.As(err, &st) {
		return nil
	}
	switch st.SQLState() {
	case "23505":
		return ErrUnique
	case "23503":
		return ErrForeignKey
	case "23514":
		return ErrCheck
	}
	return nil
}

// classifySQLite matches the constraint messages surfaced by modernc
// and mattn.
func classifySQLite(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return ErrUnique
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return ErrForeignKey
	case strings.Contains(msg, "CHECK constraint failed"):
		return ErrCheck
	}
	return nil
}
