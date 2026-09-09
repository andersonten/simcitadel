package steps

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// TestExecuteInboxAssert_ConfiguredColumns_ReachTheSelectClause holds SDD
// section 8.4, which makes every inbox column name a per-target setting. The
// column resolver has its own test, but a hardcoded SELECT keeps that test
// green while the query ignores the configuration, so the query text is read
// here. A compiled-in name queries a column the service does not have, and the
// step then fails for a reason the scenario cannot see.
func TestExecuteInboxAssert_ConfiguredColumns_ReachTheSelectClause(t *testing.T) {
	recorder := &queryRecorder{}
	handle := openRecordingDB(t, recorder)

	rc := &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				AppDB: &config.DatabaseTarget{
					Driver: "postgres",
					Inbox: &config.InboxConfig{
						Table:             "svc_inbox",
						MessageIDColumn:   "msg_id",
						ProcessedAtColumn: "handled_at",
						RetryCountColumn:  "attempts",
						LastErrorColumn:   "failure",
					},
				},
			},
		},
		DBs:      map[string]*sql.DB{"app-db": handle},
		Bindings: make(map[string]any),
	}

	step := &scenario.Step{
		Name:  "the inbox row exists",
		Type:  "inbox/assert",
		Where: map[string]any{"messageId": "m-1"},
	}

	if _, err := probeInboxRows(context.Background(), step, rc); err != nil {
		t.Fatalf("probeInboxRows: %v", err)
	}

	queries := recorder.all()
	if len(queries) != 1 {
		t.Fatalf("the step prepared %d statement(s), want 1: %v", len(queries), queries)
	}
	query := queries[0]

	for _, name := range []string{"svc_inbox", "msg_id", "handled_at", "attempts", "failure"} {
		if !strings.Contains(query, name) {
			t.Errorf("the query does not name the configured %q: %s", name, query)
		}
	}
	for _, name := range []string{"message_id", "processed_at", "retry_count", "last_error"} {
		if strings.Contains(query, name) {
			t.Errorf("the query carries the compiled-in column %q: %s", name, query)
		}
	}
}

// TestExecuteInboxAssert_EachDriver_GetsItsOwnPlaceholder holds the inbox query
// to the dialect of its target. SDD section 10.2 keeps a SQL Server application
// database behind the cdc-sqlserver profile, and SQL Server reads the marker
// @p1 and rejects $1. A compiled-in marker makes inbox/assert fail on every
// SQL Server target with a syntax error that names no cause.
func TestExecuteInboxAssert_EachDriver_GetsItsOwnPlaceholder(t *testing.T) {
	cases := []struct {
		driver  string
		want    string
		unwant  string
		dialect string
	}{
		{driver: "postgres", want: "$1", unwant: "@p1", dialect: "Postgres"},
		{driver: "sqlserver", want: "@p1", unwant: "$1", dialect: "SQL Server"},
		{driver: "mssql", want: "@p1", unwant: "$1", dialect: "SQL Server"},
	}

	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			recorder := &queryRecorder{}
			handle := openRecordingDB(t, recorder)

			rc := &RunContext{
				Config: &config.CitadelConfig{
					Targets: config.TargetsConfig{
						AppDB: &config.DatabaseTarget{
							Driver: tc.driver,
							Inbox: &config.InboxConfig{
								Table:           "svc_inbox",
								MessageIDColumn: "msg_id",
							},
						},
					},
				},
				DBs:      map[string]*sql.DB{"app-db": handle},
				Bindings: make(map[string]any),
			}

			step := &scenario.Step{
				Name:  "the inbox row exists",
				Type:  "inbox/assert",
				Where: map[string]any{"messageId": "m-1"},
			}

			if _, err := probeInboxRows(context.Background(), step, rc); err != nil {
				t.Fatalf("probeInboxRows: %v", err)
			}

			queries := recorder.all()
			if len(queries) != 1 {
				t.Fatalf("the step prepared %d statement(s), want 1: %v", len(queries), queries)
			}
			query := queries[0]

			if !strings.Contains(query, tc.want) {
				t.Errorf("the %s query holds no %s marker: %s", tc.dialect, tc.want, query)
			}
			if strings.Contains(query, tc.unwant) {
				t.Errorf("the %s query holds the %s marker of the other dialect: %s", tc.dialect, tc.unwant, query)
			}
		})
	}
}

// TestExecuteInboxAssert_UnusableDriver_ReturnsAnError proves the step reports
// a driver it cannot map, rather than falling back to one dialect in silence.
func TestExecuteInboxAssert_UnusableDriver_ReturnsAnError(t *testing.T) {
	recorder := &queryRecorder{}
	handle := openRecordingDB(t, recorder)

	rc := &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				AppDB: &config.DatabaseTarget{
					Driver: "oracle",
					Inbox:  &config.InboxConfig{Table: "svc_inbox", MessageIDColumn: "msg_id"},
				},
			},
		},
		DBs:      map[string]*sql.DB{"app-db": handle},
		Bindings: make(map[string]any),
	}

	step := &scenario.Step{
		Name:  "the inbox row exists",
		Type:  "inbox/assert",
		Where: map[string]any{"messageId": "m-1"},
	}

	_, err := probeInboxRows(context.Background(), step, rc)
	if err == nil {
		t.Fatal("inbox/assert accepted the driver \"oracle\" and chose a dialect on its own")
	}
	if !strings.Contains(err.Error(), "oracle") {
		t.Errorf("the error does not name the driver it cannot map: %v", err)
	}
}
