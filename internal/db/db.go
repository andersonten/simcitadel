// Package db opens database connections for configured targets and holds the
// shared row scanning helper. This package registers the Postgres driver and
// the SQL Server driver, and it is the only place in the module that does so.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
)

// The driver names that the two adapters register themselves under.
const (
	pgxDriver       = "pgx"
	sqlServerDriver = "sqlserver"
)

// Dialect names the SQL dialect that a driver speaks. A statement that Citadel
// builds itself must match the dialect of its target, because the placeholder
// marker and the system catalog names differ between the two engines.
type Dialect string

// The dialects that Citadel supports. SDD section 10.2 needs both, because the
// shadow is always Postgres and the application database of a team can be SQL
// Server behind the cdc-sqlserver profile.
const (
	DialectPostgres  Dialect = "postgres"
	DialectSQLServer Dialect = "sqlserver"
)

// Placeholder returns the bound parameter marker for the given position.
// Postgres counts a marker as $1, and SQL Server counts it as @p1. The
// position starts at 1.
func (d Dialect) Placeholder(position int) string {
	if d == DialectSQLServer {
		return fmt.Sprintf("@p%d", position)
	}
	return fmt.Sprintf("$%d", position)
}

// NormalizeDriver maps a configuration driver name to a registered driver name.
// The configuration file uses "postgres", which the pgx stdlib adapter
// registers as "pgx". It uses "sqlserver" or "mssql" for SQL Server, and both
// resolve to the "sqlserver" driver, which reads the @p1 marker.
func NormalizeDriver(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "postgres", "postgresql", "pgx":
		return pgxDriver, nil
	case "sqlserver", "mssql":
		return sqlServerDriver, nil
	case "":
		return "", fmt.Errorf("driver is empty")
	default:
		return "", fmt.Errorf("unsupported driver %q, expect postgres or sqlserver", name)
	}
}

// DialectOf returns the dialect of a configuration driver name. DialectOf
// returns an error for a name that NormalizeDriver rejects.
func DialectOf(name string) (Dialect, error) {
	normalized, err := NormalizeDriver(name)
	if err != nil {
		return "", err
	}
	if normalized == sqlServerDriver {
		return DialectSQLServer, nil
	}
	return DialectPostgres, nil
}

// Open opens a connection pool for the given driver and DSN and verifies it
// with a ping. Open returns an error when the driver is unsupported, the DSN
// is unusable, or the database does not answer.
func Open(ctx context.Context, driver, dsn string) (*sql.DB, error) {
	normalized, err := NormalizeDriver(driver)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	handle, err := sql.Open(normalized, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database with driver %s: %w", normalized, err)
	}

	if err := handle.PingContext(ctx); err != nil {
		handle.Close()
		return nil, fmt.Errorf("ping database with driver %s: %w", normalized, err)
	}

	return handle, nil
}

// ScanRows reads every row into a map keyed by column name.
func ScanRows(rows *sql.Rows) ([]map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = normalizeValue(values[i])
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	return result, nil
}

// normalizeValue converts driver byte slices to strings so that expectation
// comparison sees a JSON-comparable value.
func normalizeValue(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}
