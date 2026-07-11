package query

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// Insert writes row into table and returns the row's primary key
// under the package's conventional id column rule.
//
// A zero id is omitted so the database assigns it. A non-zero id is
// inserted and returned. For Postgres and non-integer ids, use raw
// INSERT RETURNING with [One].
//
//	type Returned struct{ ID int64 }
//	row, err := query.One[Returned](ctx, db,
//	    "INSERT INTO users (email) VALUES ($1) RETURNING id", email)
func Insert(ctx context.Context, db Executor, d Dialect, table string, row any) (int64, error) {
	if err := checkTable("query.Insert", table); err != nil {
		return 0, err
	}
	typ := reflect.TypeOf(row)
	if typ == nil || typ.Kind() != reflect.Struct {
		return 0, fmt.Errorf("query.Insert: row must be a struct, got %v", reflect.TypeOf(row))
	}
	fm, err := getFieldMap(typ)
	if err != nil {
		return 0, err
	}
	if err := checkColumns("query.Insert", fm, typ); err != nil {
		return 0, err
	}
	if err := checkWritable("query.Insert", fm, typ); err != nil {
		return 0, err
	}
	val := reflect.ValueOf(row)

	cols, placeholders, args := buildInsertParts(fm, val)
	var query string
	if len(cols) == 0 {
		query = fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", table)
	} else {
		query = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	}
	query, err = finalize("query.Insert", d, query)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, classify(err)
	}

	if fm.pkIndex >= 0 {
		pkVal := val.Field(fm.fields[fm.pkIndex].index)
		if !pkVal.IsZero() {
			return pkAsInt64(pkVal), nil
		}
	}
	id, _ := res.LastInsertId() // 0 on drivers without it (e.g. pgx)
	return id, nil
}

// checkColumns rejects row structs that contribute no generated
// columns.
func checkColumns(fn string, fm *fieldMap, typ reflect.Type) error {
	if len(fm.fields) == 0 {
		return fmt.Errorf("%w: %s: type %s has no columns to write (no exported fields, or every field is skipped with `db:\"-\"`)", ErrNoColumns, fn, typ)
	}
	return nil
}

// pkAsInt64 returns 0 when the supplied PK has no exact int64 form.
func pkAsInt64(v reflect.Value) int64 {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := v.Uint()
		if u > math.MaxInt64 {
			return 0
		}
		return int64(u)
	default:
		return 0
	}
}

// InsertMany writes a slice of structs with one multi-row INSERT.
//
// If every id is zero, the id column is omitted. If any id is
// non-zero, all ids are written as supplied.
func InsertMany(ctx context.Context, db Executor, d Dialect, table string, rows any) error {
	if err := checkTable("query.InsertMany", table); err != nil {
		return err
	}
	val := reflect.ValueOf(rows)
	if val.Kind() != reflect.Slice {
		return fmt.Errorf("query.InsertMany: rows must be a slice, got %s", val.Kind())
	}
	elemType := val.Type().Elem()
	if elemType.Kind() != reflect.Struct {
		return fmt.Errorf("query.InsertMany: slice element must be a struct, got %s", elemType.Kind())
	}
	fm, err := getFieldMap(elemType)
	if err != nil {
		return err
	}
	if err := checkColumns("query.InsertMany", fm, elemType); err != nil {
		return err
	}
	if err := checkWritable("query.InsertMany", fm, elemType); err != nil {
		return err
	}
	if err := checkDialect("query.InsertMany", d); err != nil {
		return err
	}
	if val.Len() == 0 {
		return nil
	}

	includePK := fm.pkIndex < 0
	if !includePK {
		for i := 0; i < val.Len(); i++ {
			if !val.Index(i).Field(fm.fields[fm.pkIndex].index).IsZero() {
				includePK = true
				break
			}
		}
	}

	cols := []string{}
	colFieldIdx := []int{} // fields[].index for each chosen column, in order
	for i, f := range fm.fields {
		if !includePK && i == fm.pkIndex {
			continue
		}
		cols = append(cols, f.column)
		colFieldIdx = append(colFieldIdx, f.index)
	}
	if len(cols) == 0 {
		return fmt.Errorf("%w: query.InsertMany: every row of %s would insert only an auto-assigned id — supply a non-PK column or use Insert per row",
			ErrNoColumns, elemType)
	}

	placeholders := "(" + strings.Repeat("?, ", len(cols)-1) + "?)"
	values := make([]string, val.Len())
	args := make([]any, 0, val.Len()*len(cols))
	for r := 0; r < val.Len(); r++ {
		rv := val.Index(r)
		for _, fi := range colFieldIdx {
			args = append(args, argOf(rv.Field(fi).Interface()))
		}
		values[r] = placeholders
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s",
		table, strings.Join(cols, ", "), strings.Join(values, ", "))
	query, err = finalize("query.InsertMany", d, query)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		return classify(err)
	}
	return nil
}

// Update sets every column in row and returns the affected row count.
// Args after row bind to where placeholders in order.
//
//	n, err := query.Update(ctx, db, d, "users", "id = ?",
//	    UserUpdate{Email: "x", Name: "y"}, userID)
//
// For partial updates, define a smaller struct with only the columns
// to change. Affected rows come from [database/sql.Result.RowsAffected].
//
// where is trusted SQL. Use ? placeholders for values, and use
// "1 = 1" to update every row deliberately.
func Update(ctx context.Context, db Executor, d Dialect, table, where string, row any, whereArgs ...any) (int64, error) {
	if err := checkTable("query.Update", table); err != nil {
		return 0, err
	}
	if err := checkWhere("query.Update", where); err != nil {
		return 0, err
	}
	typ := reflect.TypeOf(row)
	if typ == nil || typ.Kind() != reflect.Struct {
		return 0, fmt.Errorf("query.Update: row must be a struct, got %v", reflect.TypeOf(row))
	}
	fm, err := getFieldMap(typ)
	if err != nil {
		return 0, err
	}
	if err := checkColumns("query.Update", fm, typ); err != nil {
		return 0, err
	}
	if err := checkWritable("query.Update", fm, typ); err != nil {
		return 0, err
	}
	val := reflect.ValueOf(row)

	sets := make([]string, len(fm.fields))
	args := make([]any, 0, len(fm.fields)+len(whereArgs))
	for i, f := range fm.fields {
		sets[i] = f.column + " = ?"
		args = append(args, argOf(val.Field(f.index).Interface()))
	}
	args = append(args, normArgs(whereArgs)...)
	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		table, strings.Join(sets, ", "), where)
	query, err = finalize("query.Update", d, query)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, classify(err)
	}
	return res.RowsAffected()
}

// Delete removes rows matching where and returns the affected row
// count from [database/sql.Result.RowsAffected].
//
// where is trusted SQL. Use ? placeholders for values, and use
// "1 = 1" to delete every row deliberately.
func Delete(ctx context.Context, db Executor, d Dialect, table, where string, whereArgs ...any) (int64, error) {
	if err := checkTable("query.Delete", table); err != nil {
		return 0, err
	}
	if err := checkWhere("query.Delete", where); err != nil {
		return 0, err
	}
	stmt := fmt.Sprintf("DELETE FROM %s WHERE %s", table, where)
	stmt, err := finalize("query.Delete", d, stmt)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, stmt, normArgs(whereArgs)...)
	if err != nil {
		return 0, classify(err)
	}
	return res.RowsAffected()
}

// buildInsertParts excludes a zero auto-PK column.
func buildInsertParts(fm *fieldMap, val reflect.Value) (cols []string, placeholders []string, args []any) {
	for i, f := range fm.fields {
		if i == fm.pkIndex && val.Field(f.index).IsZero() {
			continue
		}
		cols = append(cols, f.column)
		placeholders = append(placeholders, "?")
		args = append(args, argOf(val.Field(f.index).Interface()))
	}
	return cols, placeholders, args
}
