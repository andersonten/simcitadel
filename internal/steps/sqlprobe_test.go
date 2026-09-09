package steps

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the sql probe source of SDD section 6.5, which returned
// "database connection not available" for every query before. It uses a
// database/sql driver defined here rather than a new dependency, because the
// behaviour under test is the wiring between the probe and the row set, not the
// behaviour of any real database.

type stubDriver struct{ rows *stubRows }

func (d stubDriver) Open(string) (driver.Conn, error) { return stubConn{rows: d.rows}, nil }

type stubConn struct{ rows *stubRows }

func (c stubConn) Prepare(query string) (driver.Stmt, error) { return stubStmt{rows: c.rows}, nil }
func (c stubConn) Close() error                              { return nil }
func (c stubConn) Begin() (driver.Tx, error)                 { return nil, io.EOF }

type stubStmt struct{ rows *stubRows }

func (s stubStmt) Close() error  { return nil }
func (s stubStmt) NumInput() int { return -1 }
func (s stubStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}
func (s stubStmt) Query([]driver.Value) (driver.Rows, error) {
	copied := *s.rows
	copied.next = 0
	return &copied, nil
}

type stubRows struct {
	cols []string
	data [][]driver.Value
	next int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.next >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.next])
	r.next++
	return nil
}

// openStubDB registers a driver that answers every query with the given rows.
func openStubDB(t *testing.T, cols []string, data [][]driver.Value) *sql.DB {
	t.Helper()
	name := "citadel-stub-" + strings.ReplaceAll(t.Name(), "/", "-")
	sql.Register(name, stubDriver{rows: &stubRows{cols: cols, data: data}})
	handle, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open stub database: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return handle
}

func sqlRunContext(t *testing.T, handle *sql.DB) *RunContext {
	t.Helper()
	return &RunContext{
		Config:   &config.CitadelConfig{},
		DBs:      map[string]*sql.DB{"app-db": handle},
		Bindings: make(map[string]any),
	}
}

// TestExecuteProbe_SQLSource_ReturnsTheRows holds the sql row of the source
// table in SDD section 6.5. The source must query the database rather than
// report that no connection is available.
func TestExecuteProbe_SQLSource_ReturnsTheRows(t *testing.T) {
	handle := openStubDB(t,
		[]string{"message_id", "retry_count"},
		[][]driver.Value{
			{"m-1", int64(0)},
			{"m-2", int64(1)},
		},
	)

	step := &scenario.Step{
		Name:   "read the inbox",
		Type:   "probe",
		Target: "app-db",
		SQL:    &scenario.SQLSource{Query: "select message_id, retry_count from inbox"},
		Expect: &scenario.Expect{Rows: intPtr(2)},
	}

	result, err := ExecuteProbe(context.Background(), step, sqlRunContext(t, handle))
	if err != nil {
		t.Fatalf("ExecuteProbe: %v", err)
	}
	if result.Err != nil {
		t.Fatalf("probe reported an error: %v", result.Err)
	}

	rows, ok := result.Response.([]any)
	if !ok {
		t.Fatalf("response is %T, want a row list", result.Response)
	}
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2", len(rows))
	}
	first, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("row is %T, want a column map", rows[0])
	}
	if first["message_id"] != "m-1" {
		t.Errorf("message_id = %v, want m-1", first["message_id"])
	}
}

// TestExecuteWithRetry_SQLSource_EvaluatesRowsColumnsAndNotNull holds the inbox
// assertion of SDD section 8.4 end to end through the probe.
func TestExecuteWithRetry_SQLSource_EvaluatesRowsColumnsAndNotNull(t *testing.T) {
	handle := openStubDB(t,
		[]string{"message_id", "retry_count", "processed_at"},
		[][]driver.Value{{"m-1", int64(0), "2026-08-19T09:00:00Z"}},
	)

	step := &scenario.Step{
		Name:   "assert the inbox row",
		Type:   "probe",
		Target: "app-db",
		SQL:    &scenario.SQLSource{Query: "select * from inbox"},
		Expect: &scenario.Expect{
			Rows:    intPtr(1),
			Columns: map[string]any{"retry_count": 0},
			NotNull: []string{"processed_at"},
		},
	}

	result, err := ExecuteWithRetry(context.Background(), step, sqlRunContext(t, handle), config.DefaultsConfig{})
	if err != nil {
		t.Fatalf("ExecuteWithRetry: %v", err)
	}
	if !result.Pass {
		t.Errorf("a conforming inbox row failed: %s", result.Message)
	}
}

// TestExecuteWithRetry_SQLSource_WrongRowCountFails keeps the test above from
// passing for the wrong reason.
func TestExecuteWithRetry_SQLSource_WrongRowCountFails(t *testing.T) {
	handle := openStubDB(t,
		[]string{"message_id"},
		[][]driver.Value{{"m-1"}},
	)

	step := &scenario.Step{
		Name:   "expect two rows",
		Type:   "probe",
		Target: "app-db",
		SQL:    &scenario.SQLSource{Query: "select * from inbox"},
		Expect: &scenario.Expect{Rows: intPtr(2)},
	}

	result, err := ExecuteWithRetry(context.Background(), step, sqlRunContext(t, handle), config.DefaultsConfig{})
	if err != nil {
		t.Fatalf("ExecuteWithRetry: %v", err)
	}
	if result.Pass {
		t.Error("one row passed an expectation of two")
	}
}

func intPtr(v int) *int { return &v }
