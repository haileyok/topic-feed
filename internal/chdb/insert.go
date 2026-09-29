package chdb

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Columns lists a row type's `ch` tags, in field order. Inserts name their columns, so
// adding a column to a table never breaks a running service built before the change.
func Columns[T any]() string {
	t := reflect.TypeFor[T]()
	var cols []string
	for i := 0; i < t.NumField(); i++ {
		if tag := t.Field(i).Tag.Get("ch"); tag != "" && tag != "-" {
			cols = append(cols, tag)
		}
	}
	return strings.Join(cols, ", ")
}

// Insert writes rows (structs with `ch` tags) to table in one batch.
func Insert[T any](ctx context.Context, conn driver.Conn, table string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+table+" ("+Columns[T]()+")")
	if err != nil {
		return fmt.Errorf("prepare %s: %w", table, err)
	}
	for i := range rows {
		if err := batch.AppendStruct(&rows[i]); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("append %s: %w", table, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send %s: %w", table, err)
	}
	return nil
}
