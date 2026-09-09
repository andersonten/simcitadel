package runner

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/andersonten/simcitadel/internal/config"
	citadeldb "github.com/andersonten/simcitadel/internal/db"
)

// outboxRowCap bounds the row identity snapshot. A Citadel outbox is a test
// fixture and holds a handful of rows, so this only stops the check reading a
// table that a caller pointed at by mistake.
const outboxRowCap = 20000

// OutboxOps is the state of the outbox table at one moment. The runner reads it
// after reset and compares it after the assertions, so a difference describes the
// run window and nothing before it.
type OutboxOps struct {
	// Table is the table the snapshot belongs to. It is empty when the check has
	// nothing to read, which is how an unconfigured environment stays quiet.
	Table string

	// Rows maps the primary key of every outbox row to its xmin, the transaction
	// that last wrote it. An UPDATE gives a row a new xmin and a DELETE removes it,
	// so this pair detects both without reading a log and without a race.
	Rows map[string]string

	// Capped records that the table held more rows than the snapshot reads. The row
	// comparison is then unavailable and only the counters below apply.
	Capped bool

	// Updates and Deletes are the cumulative tuple counters Postgres keeps for the
	// table. They cover a row that was inserted and then mutated inside the same
	// run, which the row comparison cannot see, because such a row is absent from
	// the baseline.
	Updates int64
	Deletes int64
}

// ReadOutboxOps snapshots the outbox table. The runner calls it after reset, so
// what it returns is the baseline of the run window.
//
// SDD section 4.11 describes this check as a scan of the Debezium connector log for
// EventRouter invalid-operation warnings. That log is not reachable: the connector
// runs in its own container, the Debezium Server image serves no metrics endpoint,
// and a Kubernetes deployment shares no filesystem with the machine running citadel.
// So the check reads the operation from the database that performed it, which is the
// thing the log line reports rather than a report of it.
func ReadOutboxOps(ctx context.Context, cfg *config.CitadelConfig, databases map[string]*sql.DB) (OutboxOps, error) {
	table, db, ok := outboxTarget(cfg, databases)
	if !ok {
		return OutboxOps{}, nil
	}
	return readOutboxSnapshot(ctx, db, table)
}

// CheckSkippedOutbox reports an UPDATE or a DELETE against the outbox table during
// the run window.
//
// Section 10.1 keeps table.op.invalid.behavior at the production default of warn, so
// the EventRouter skips such an operation rather than fail, and nothing else reports
// it. The row changes, no event is produced, and the scenario fails on an await with
// no evidence of the cause.
func CheckSkippedOutbox(ctx context.Context, cfg *config.CitadelConfig, databases map[string]*sql.DB, baseline OutboxOps) error {
	table, db, ok := outboxTarget(cfg, databases)
	if !ok {
		return nil
	}
	if baseline.Table == "" {
		// No baseline was read, so there is no window to compare against. Reporting
		// nothing is the only honest answer.
		return nil
	}
	if baseline.Table != table {
		return fmt.Errorf("the outbox baseline is for table %s and the check reads %s", baseline.Table, table)
	}

	current, err := readOutboxSnapshot(ctx, db, table)
	if err != nil {
		return err
	}

	updated, deleted := diffOutboxRows(baseline, current)

	// The counters are cumulative, and a backend flushes them when its transaction
	// ends, so they can lag a mutation by up to a second. They are the only signal
	// for a row that was inserted and then mutated inside one run, because such a
	// row is not in the baseline. A lag therefore costs a late report and never a
	// false one: a counter does not move without an operation behind it.
	extraUpdates := current.Updates - baseline.Updates - int64(len(updated))
	extraDeletes := current.Deletes - baseline.Deletes - int64(len(deleted))

	if len(updated) == 0 && len(deleted) == 0 && extraUpdates <= 0 && extraDeletes <= 0 {
		return nil
	}

	var parts []string
	if len(updated) > 0 {
		parts = append(parts, "updated row(s) "+joinRows(updated))
	}
	if len(deleted) > 0 {
		parts = append(parts, "deleted row(s) "+joinRows(deleted))
	}
	// A row that was written and then mutated inside one run is counted rather than
	// named, because it is absent from the baseline that holds the names.
	if extraUpdates > 0 {
		parts = append(parts, fmt.Sprintf("%d further UPDATE(s)", extraUpdates))
	}
	if extraDeletes > 0 {
		parts = append(parts, fmt.Sprintf("%d further DELETE(s)", extraDeletes))
	}

	// A StepFailureError exits 1, which is where SDD section 4.4 puts a finding
	// about the application under test. The envelope conformance check of the same
	// section reports the same way, and for the same reason: the environment is
	// working and the application or its fixture is not.
	return &StepFailureError{Msg: fmt.Sprintf(
		"skipped outbox operation: %s: %s; the Debezium EventRouter skips an operation that is not an INSERT "+
			"and only writes a log line, so no event was produced for it; write a new outbox row instead of "+
			"changing one",
		table, strings.Join(parts, ", "))}
}

// joinRows lists at most three row ids and counts the rest, so a failure caused by
// a mass update stays readable.
func joinRows(rows []string) string {
	if len(rows) <= 3 {
		return strings.Join(rows, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(rows[:3], ", "), len(rows)-3)
}

// diffOutboxRows returns the rows that changed and the rows that disappeared.
func diffOutboxRows(baseline, current OutboxOps) (updated, deleted []string) {
	if baseline.Capped || current.Capped {
		return nil, nil
	}
	for id, was := range baseline.Rows {
		now, present := current.Rows[id]
		switch {
		case !present:
			deleted = append(deleted, id)
		case now != was:
			updated = append(updated, id)
		}
	}
	// A map has no order, and a failure message that changes between runs is
	// harder to compare than one that does not.
	sort.Strings(updated)
	sort.Strings(deleted)
	return updated, deleted
}

// readOutboxSnapshot reads the row identities and the tuple counters.
func readOutboxSnapshot(ctx context.Context, db *sql.DB, table string) (OutboxOps, error) {
	snapshot := OutboxOps{Table: table, Rows: make(map[string]string)}

	// A table name cannot travel as a bound parameter, so it is validated and
	// quoted instead. ResetTruncate does the same for the same reason.
	if err := validateTableName(table); err != nil {
		return OutboxOps{}, fmt.Errorf("the outbox table: %w", err)
	}

	// xmin is the transaction that last wrote the row, so a change to it is an
	// update. It is a system column and needs a cast to be scanned as text.
	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT id::text, xmin::text FROM %s LIMIT %d",
		quoteTableName(table), outboxRowCap+1))
	if err != nil {
		return OutboxOps{}, &EnvironmentError{Msg: fmt.Sprintf("read the outbox rows of %s: %v", table, err)}
	}
	defer rows.Close()

	for rows.Next() {
		var id, xmin string
		if err := rows.Scan(&id, &xmin); err != nil {
			return OutboxOps{}, &EnvironmentError{Msg: fmt.Sprintf("read an outbox row of %s: %v", table, err)}
		}
		snapshot.Rows[id] = xmin
	}
	if err := rows.Err(); err != nil {
		return OutboxOps{}, &EnvironmentError{Msg: fmt.Sprintf("read the outbox rows of %s: %v", table, err)}
	}
	if len(snapshot.Rows) > outboxRowCap {
		snapshot.Capped = true
		snapshot.Rows = nil
		slog.Warn("the outbox table holds more rows than the skipped-operation check compares",
			"table", table, "cap", outboxRowCap)
	}

	counters, err := readTupleCounters(ctx, db, table)
	if err != nil {
		return OutboxOps{}, err
	}
	snapshot.Updates, snapshot.Deletes = counters.Updates, counters.Deletes

	return snapshot, nil
}

// readTupleCounters reads n_tup_upd and n_tup_del for one table.
func readTupleCounters(ctx context.Context, db *sql.DB, table string) (OutboxOps, error) {
	schema, name := splitTableName(table)

	var ops OutboxOps
	err := db.QueryRowContext(ctx, `
		SELECT coalesce(sum(n_tup_upd), 0), coalesce(sum(n_tup_del), 0)
		FROM pg_stat_all_tables
		WHERE schemaname = $1 AND relname = $2`,
		schema, name).Scan(&ops.Updates, &ops.Deletes)
	if err != nil {
		return OutboxOps{}, &EnvironmentError{Msg: fmt.Sprintf(
			"read the outbox operation counters for %s: %v", table, err)}
	}
	return ops, nil
}

// outboxCheckReadsAppDB reports whether the skipped-outbox check reads the
// application database in this environment.
//
// The runner asks before it opens its connections. SDD section 4.11 runs the
// check after the assertions of every scenario, so the run reaches the
// application database even when no step names it. A check that holds no
// connection reports nothing, and a scenario that writes no SQL left it silent.
func outboxCheckReadsAppDB(cfg *config.CitadelConfig) bool {
	if cfg == nil || cfg.Targets.DebeziumOutbox == nil || cfg.Targets.DebeziumOutbox.URL == "" {
		return false
	}
	if cfg.Targets.AppDB == nil || cfg.Targets.AppDB.DSN == "" {
		return false
	}

	// xmin and pg_stat_all_tables are Postgres. SQL Server holds the same
	// information behind different views, and SDD section 5.1 already skips several
	// checks for SQL Server, so this follows that rather than guess.
	dialect, err := citadeldb.DialectOf(cfg.Targets.AppDB.Driver)
	return err == nil && dialect == citadeldb.DialectPostgres
}

// outboxTarget resolves the table and the connection the check reads. It reports
// false when the environment runs no outbox, when no application database is
// configured, or when the driver keeps no such information.
func outboxTarget(cfg *config.CitadelConfig, databases map[string]*sql.DB) (string, *sql.DB, bool) {
	if !outboxCheckReadsAppDB(cfg) {
		return "", nil, false
	}

	db := databases["app-db"]
	if db == nil {
		return "", nil, false
	}
	return cfg.Targets.DebeziumOutbox.OutboxTableOrDefault(), db, true
}

// splitTableName splits a qualified name and defaults the schema to public.
func splitTableName(table string) (schema, name string) {
	if idx := strings.LastIndex(table, "."); idx >= 0 {
		return table[:idx], table[idx+1:]
	}
	return "public", table
}
