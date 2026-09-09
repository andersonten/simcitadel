//go:build integration

package integration

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	citadeldb "github.com/andersonten/simcitadel/internal/db"
	"github.com/andersonten/simcitadel/internal/runner"
)

// SDD section 4.11 requires the run to fail when an UPDATE or a DELETE reaches the
// outbox table. Section 10.1 keeps table.op.invalid.behavior at warn, so the
// Debezium EventRouter skips such an operation and writes a log line, and a
// scenario whose fixture mutates an outbox row instead of inserting one therefore
// produces silence: no event, no error, and an await that times out with nothing
// to explain it.
//
// The check was a stub that logged one line and returned nothing, so it could not
// fail a run. It is now read from the statistics Postgres keeps for the table,
// which is the operation itself rather than a report of it.

// outboxSchema is the schema of SDD section 10.1, reduced to what the check reads.
const outboxSchema = `
CREATE TABLE public.outbox (
	id            uuid         PRIMARY KEY,
	aggregatetype varchar(255) NOT NULL,
	aggregateid   varchar(255) NOT NULL,
	type          varchar(255) NOT NULL,
	payload       jsonb        NOT NULL,
	created_at    timestamptz  NOT NULL DEFAULT now()
);
INSERT INTO public.outbox (id, aggregatetype, aggregateid, type, payload)
VALUES ('11111111-1111-4111-8111-111111111111',
        'public.orchestration-engine.Manuscript.v1', 'agg-1', 'Manuscript.Created', '{}');
`

// newOutboxConfig builds a config whose app-db target is a live database holding
// the outbox table.
func newOutboxConfig(t *testing.T) (*config.CitadelConfig, map[string]*sql.DB) {
	t.Helper()
	ctx := context.Background()

	dsn := startPostgres(t)
	db, err := citadeldb.Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.ExecContext(ctx, outboxSchema); err != nil {
		t.Fatalf("create the outbox table: %v", err)
	}

	cfg := &config.CitadelConfig{}
	cfg.Targets.AppDB = &config.DatabaseTarget{Driver: "postgres", DSN: dsn}
	cfg.Targets.DebeziumOutbox = &config.DebeziumOutboxTarget{URL: "http://debezium.invalid:8080"}

	return cfg, map[string]*sql.DB{"app-db": db}
}

// TestCheckSkippedOutbox_NoMutation_Passes keeps the check from failing a run that
// did nothing wrong. An INSERT is the operation the outbox pattern uses, and the
// EventRouter routes it, so it must not be reported.
func TestCheckSkippedOutbox_NoMutation_Passes(t *testing.T) {
	ctx := context.Background()
	cfg, databases := newOutboxConfig(t)

	baseline, err := runner.ReadOutboxOps(ctx, cfg, databases)
	if err != nil {
		t.Fatalf("read the baseline: %v", err)
	}

	if _, err := databases["app-db"].ExecContext(ctx, `
		INSERT INTO public.outbox (id, aggregatetype, aggregateid, type, payload)
		VALUES ('22222222-2222-4222-8222-222222222222',
		        'public.orchestration-engine.Manuscript.v1', 'agg-2', 'Manuscript.Created', '{}')`,
	); err != nil {
		t.Fatalf("insert an outbox row: %v", err)
	}

	if err := runner.CheckSkippedOutbox(ctx, cfg, databases, baseline); err != nil {
		t.Errorf("the check failed a run whose only outbox operation was an insert: %v", err)
	}
}

// TestCheckSkippedOutbox_UpdateDuringTheRun_FailsTheRun is the behaviour section
// 4.11 requires.
func TestCheckSkippedOutbox_UpdateDuringTheRun_FailsTheRun(t *testing.T) {
	ctx := context.Background()
	cfg, databases := newOutboxConfig(t)

	baseline, err := runner.ReadOutboxOps(ctx, cfg, databases)
	if err != nil {
		t.Fatalf("read the baseline: %v", err)
	}

	// This is the fixture mistake section 4.11 describes: a mutation where an
	// insert was meant. The EventRouter skips it and the run would otherwise pass.
	if _, err := databases["app-db"].ExecContext(ctx,
		`UPDATE public.outbox SET type = 'Manuscript.Updated' WHERE aggregateid = 'agg-1'`,
	); err != nil {
		t.Fatalf("update an outbox row: %v", err)
	}

	err = runner.CheckSkippedOutbox(ctx, cfg, databases, baseline)
	if err == nil {
		t.Fatal("an UPDATE against the outbox did not fail the run, so the skip stays silent")
	}
	if !strings.Contains(err.Error(), "public.outbox") {
		t.Errorf("the failure does not name the table: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "update") {
		t.Errorf("the failure does not name the operation: %v", err)
	}
	// Section 4.11 requires the offending row id, which is why the check compares
	// row identity rather than only the tuple counters.
	if !strings.Contains(err.Error(), "11111111-1111-4111-8111-111111111111") {
		t.Errorf("the failure does not name the offending row, which section 4.11 requires: %v", err)
	}

	// The finding is about the application or its fixture and not the environment,
	// so it exits 1 like a malformed envelope rather than 3.
	if code := runner.ClassifyError(err); code != runner.ExitFailure {
		t.Errorf("a skipped outbox operation exits %d, want %d", code, runner.ExitFailure)
	}
}

// TestCheckSkippedOutbox_DeleteDuringTheRun_FailsTheRun covers the other operation
// the EventRouter skips. Section 10.1 keeps warn precisely so an outbox cleanup job
// cannot take the environment down, which means a DELETE is silent and this is the
// only thing that reports it.
func TestCheckSkippedOutbox_DeleteDuringTheRun_FailsTheRun(t *testing.T) {
	ctx := context.Background()
	cfg, databases := newOutboxConfig(t)

	baseline, err := runner.ReadOutboxOps(ctx, cfg, databases)
	if err != nil {
		t.Fatalf("read the baseline: %v", err)
	}

	if _, err := databases["app-db"].ExecContext(ctx,
		`DELETE FROM public.outbox WHERE aggregateid = 'agg-1'`); err != nil {
		t.Fatalf("delete an outbox row: %v", err)
	}

	err = runner.CheckSkippedOutbox(ctx, cfg, databases, baseline)
	if err == nil {
		t.Fatal("a DELETE against the outbox did not fail the run, so the skip stays silent")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "delete") {
		t.Errorf("the failure does not name the operation: %v", err)
	}
}

// TestCheckSkippedOutbox_MutationBeforeTheRun_IsNotReported proves the check reads
// the run window and not the lifetime of the database. The counters Postgres keeps
// are cumulative, so a check that read them without a baseline would fail every run
// after the first mutation forever.
func TestCheckSkippedOutbox_MutationBeforeTheRun_IsNotReported(t *testing.T) {
	ctx := context.Background()
	cfg, databases := newOutboxConfig(t)

	if _, err := databases["app-db"].ExecContext(ctx,
		`UPDATE public.outbox SET type = 'Before' WHERE aggregateid = 'agg-1'`); err != nil {
		t.Fatalf("update an outbox row: %v", err)
	}

	// The baseline is taken after the mutation, which is what reset does.
	baseline, err := runner.ReadOutboxOps(ctx, cfg, databases)
	if err != nil {
		t.Fatalf("read the baseline: %v", err)
	}

	if err := runner.CheckSkippedOutbox(ctx, cfg, databases, baseline); err != nil {
		t.Errorf("the check reported a mutation that happened before the run: %v", err)
	}
}

// TestCheckSkippedOutbox_NoOutboxConfigured_Passes keeps the check quiet in an
// environment that runs no outbox at all.
func TestCheckSkippedOutbox_NoOutboxConfigured_Passes(t *testing.T) {
	ctx := context.Background()
	cfg := &config.CitadelConfig{}

	baseline, err := runner.ReadOutboxOps(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("read the baseline with no outbox configured: %v", err)
	}
	if err := runner.CheckSkippedOutbox(ctx, cfg, nil, baseline); err != nil {
		t.Errorf("the check failed a run with no outbox configured: %v", err)
	}
}
