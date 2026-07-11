package query

import (
	"context"
	"database/sql"
	"fmt"
)

// Tx runs fn inside a database transaction.
//
//	err := query.Tx(ctx, db, func(tx *sql.Tx) error {
//	    if _, err := query.Insert(ctx, tx, d, "users", u); err != nil {
//	        return err
//	    }
//	    return query.InsertMany(ctx, tx, d, "audit", events)
//	})
//
// It commits on nil, rolls back on error, and rolls back before
// re-panicking.
func Tx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("query.Tx: begin: %w", err)
	}
	panicked := true
	defer func() {
		if panicked {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		panicked = false
		if rerr := tx.Rollback(); rerr != nil {
			return fmt.Errorf("query.Tx: %w (rollback: %v)", err, rerr)
		}
		return err
	}
	panicked = false
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("query.Tx: commit: %w", classify(err))
	}
	return nil
}
