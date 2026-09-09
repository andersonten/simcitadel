package steps

import (
	"context"
	"database/sql"
	"fmt"

	citadeldb "github.com/andersonten/simcitadel/internal/db"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// ExecuteSQLExec runs a SQL statement against the target database.
func ExecuteSQLExec(ctx context.Context, step *scenario.Step, rc *RunContext) error {
	handle, err := resolveDB(step, rc)
	if err != nil {
		return err
	}

	query, err := resolveSQLQuery(step, rc)
	if err != nil {
		return err
	}

	_, err = handle.ExecContext(ctx, query, resolveSQLArgs(step)...)
	if err != nil {
		return fmt.Errorf("sql exec: %w", err)
	}
	return nil
}

// ExecuteSQLQuery runs a SQL query and returns the result for expectation checking.
func ExecuteSQLQuery(ctx context.Context, step *scenario.Step, rc *RunContext) (any, error) {
	handle, err := resolveDB(step, rc)
	if err != nil {
		return nil, err
	}

	query, err := resolveSQLQuery(step, rc)
	if err != nil {
		return nil, err
	}

	rows, err := handle.QueryContext(ctx, query, resolveSQLArgs(step)...)
	if err != nil {
		return nil, fmt.Errorf("sql query: %w", err)
	}
	defer rows.Close()

	results, err := citadeldb.ScanRows(rows)
	if err != nil {
		return nil, fmt.Errorf("sql query: %w", err)
	}
	return results, nil
}

func resolveDB(step *scenario.Step, rc *RunContext) (*sql.DB, error) {
	target := step.Target
	if target == "" {
		target = "app-db"
	}
	handle, ok := rc.DBs[target]
	if !ok || handle == nil {
		return nil, fmt.Errorf("sql: database %q is not connected", target)
	}
	return handle, nil
}

// resolveSQLArgs returns the parameters the statement binds. SDD section 6.5
// writes the sql source as "query or file, plus args", so an args list inside
// the sql block binds exactly as a step level args list does. A statement that
// reaches the database with its markers unbound fails on a defect in the tool
// and not on a fact about the application.
func resolveSQLArgs(step *scenario.Step) []any {
	if step.SQL != nil && len(step.SQL.Args) > 0 {
		return step.SQL.Args
	}
	return step.Args
}

func resolveSQLQuery(step *scenario.Step, rc *RunContext) (string, error) {
	if step.SQL != nil && step.SQL.File != "" {
		return readSQLFile(step.SQL.File, rc)
	}
	if step.SQL != nil && step.SQL.Query != "" {
		return step.SQL.Query, nil
	}
	if step.File != "" {
		return readSQLFile(step.File, rc)
	}
	if step.Query != "" {
		return step.Query, nil
	}
	return "", fmt.Errorf("sql: no query or file specified")
}

func readSQLFile(file string, rc *RunContext) (string, error) {
	path := rc.Config.SQLPath(file)
	rendered, err := readRenderedContent(path, rc)
	if err != nil {
		return "", fmt.Errorf("sql file: %w", err)
	}
	return rendered, nil
}
