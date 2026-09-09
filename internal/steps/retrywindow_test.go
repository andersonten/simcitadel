package steps

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file holds the retry window rules of SDD section 4.6 and the selector
// rules of section 8.2, including the arguments each SQL source carries.

// TestExecuteCaptured_StepSelectorsWithACapturedBlock_ReachTheCaptureService
// holds the capture query of SDD section 8.2 to every selector the step wrote.
// A step that names a captured block as well as a step level selector must send
// both. A selector left behind widens the query, and a widened query lets an
// assertion match a message the author excluded.
func TestExecuteCaptured_StepSelectorsWithACapturedBlock_ReachTheCaptureService(t *testing.T) {
	srv, queries := captureQueryServer(t, `[{"exchange":"other","routingKey":"other","body":{"id":"e1"}}]`)

	rc := notCapturedContext(t, srv.URL, srv.Client())
	count := 1
	step := &scenario.Step{
		Name:         "the article event was captured",
		Type:         "event/captured",
		Exchange:     "public.orchestration-engine.Article.v1",
		RoutingKey:   "public.orchestration-engine.Article.v1",
		HeaderFilter: map[string]string{"aggregateId": "515150"},
		Captured:     &scenario.CapturedQuery{EventType: "EditorAssigned"},
		Expect:       &scenario.Expect{Count: &count},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the step failed against a capture service that answered one message")
	}

	sent := queries()
	if len(sent) != 1 {
		t.Fatalf("the step made %d capture queries, want 1", len(sent))
	}
	for _, want := range []struct{ key, value string }{
		{"exchange", "public.orchestration-engine.Article.v1"},
		{"routingKey", "public.orchestration-engine.Article.v1"},
		{"eventType", "EditorAssigned"},
		{"header.aggregateId", "515150"},
	} {
		if got := sent[0].Get(want.key); got != want.value {
			t.Errorf("the capture query carried %s=%q, want %q", want.key, got, want.value)
		}
	}
}

// TestExecute_AwaitWhoseStepTimeoutClosesTheWindow_ReportsAnAssertionFailure
// holds the per-step ceiling of SDD section 6.3 to the report of section 4.6. A
// window that closes is an assertion failure with its attempt count, which is
// exit 1. Reporting it as a target that went away sends the failure to the
// wrong team and throws away the value the probe last read.
func TestExecute_AwaitWhoseStepTimeoutClosesTheWindow_ReportsAnAssertionFailure(t *testing.T) {
	var calls int
	var mu sync.Mutex
	shutdown := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if !first {
			// Every later request stays open until the step ceiling closes it,
			// so the window always closes during a request and the test is
			// deterministic.
			select {
			case <-r.Context().Done():
			case <-shutdown:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"status":"CLOSED"}}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(shutdown) })

	rc := graphQLProbeContext(t, srv.URL, srv.Client())
	step := &scenario.Step{
		Name:    "wait for the task to open",
		Type:    "await",
		Target:  "app",
		Timeout: "150ms",
		GraphQL: &scenario.GraphQLSource{Query: "{ status }"},
		Expect:  &scenario.Expect{JSONPath: "$.data.status", Equals: "OPEN"},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("the step ceiling of section 6.3 reported an unreachable target: %v", err)
	}
	if result.Pass {
		t.Fatalf("the step passed against a target that never answered OPEN")
	}
	if result.Attempts < 1 {
		t.Errorf("attempts = %d, want the count of section 4.6", result.Attempts)
	}
	if result.Expect == nil || result.Expect.Actual == nil {
		t.Fatalf("the failure carried no actual value, so the last read was lost")
	}
	if got, ok := result.Expect.Actual.(string); !ok || got != "CLOSED" {
		t.Errorf("actual = %v, want the value the probe last read", result.Expect.Actual)
	}
}

// graphQLProbeContext returns a run context whose app target is the given
// address, with a short retry interval and a long retry window, so the step
// ceiling is the only thing that closes the loop.
func graphQLProbeContext(t *testing.T, appURL string, client *http.Client) *RunContext {
	t.Helper()
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			App: &config.AppTarget{URL: appURL, GraphQLPath: "/graphql"},
		},
	}
	cfg.Defaults = awaitDefaults(t, "10s", "1ms")
	return &RunContext{
		Config:     cfg,
		RunID:      "run-1",
		RunStart:   time.Now(),
		HTTPClient: client,
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}
}

// awaitDefaults builds a defaults block from the two duration strings, through
// the YAML path config.DefaultsConfig accepts.
func awaitDefaults(t *testing.T, timeout, interval string) config.DefaultsConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "citadel.yaml")
	body := "version: 1\nenvironment:\n  name: local\ndefaults:\n  awaitTimeout: " +
		timeout + "\n  awaitInterval: " + interval + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Defaults
}

// TestExecuteSQLQuery_ArgsUnderTheSQLSource_ReachTheDatabase holds the sql
// source of SDD section 6.5 to its own fields. The source is "query or file,
// plus args", so an args list written inside the sql block must bind to the
// statement. Dropping it leaves the markers unbound.
func TestExecuteSQLQuery_ArgsUnderTheSQLSource_ReachTheDatabase(t *testing.T) {
	handle, recorded := bindRecordingDatabase(t)
	rc := bindRunContext(handle)

	step := &scenario.Step{
		Name:   "the article row exists",
		Type:   "sql/query",
		Target: "app-db",
		SQL: &scenario.SQLSource{
			Query: "SELECT id FROM article WHERE id = $1",
			Args:  []any{"515150"},
		},
	}

	if _, err := ExecuteSQLQuery(context.Background(), step, rc); err != nil {
		t.Fatalf("ExecuteSQLQuery: %v", err)
	}

	calls := recorded()
	if len(calls) != 1 {
		t.Fatalf("the step ran %d statements, want 1", len(calls))
	}
	if len(calls[0].args) != 1 || calls[0].args[0] != "515150" {
		t.Errorf("the statement bound %v, want the args of the sql source", calls[0].args)
	}
}

// TestExecuteSQLExec_ArgsUnderTheSQLSource_ReachTheDatabase is the sql/exec half
// of the same rule of SDD section 6.5.
func TestExecuteSQLExec_ArgsUnderTheSQLSource_ReachTheDatabase(t *testing.T) {
	handle, recorded := bindRecordingDatabase(t)
	rc := bindRunContext(handle)

	step := &scenario.Step{
		Name:   "seed the article",
		Type:   "sql/exec",
		Target: "app-db",
		SQL: &scenario.SQLSource{
			Query: "INSERT INTO article (id) VALUES ($1)",
			Args:  []any{"515150"},
		},
	}

	if err := ExecuteSQLExec(context.Background(), step, rc); err != nil {
		t.Fatalf("ExecuteSQLExec: %v", err)
	}

	calls := recorded()
	if len(calls) != 1 {
		t.Fatalf("the step ran %d statements, want 1", len(calls))
	}
	if len(calls[0].args) != 1 || calls[0].args[0] != "515150" {
		t.Errorf("the statement bound %v, want the args of the sql source", calls[0].args)
	}
}

// TestExecute_SQLQueryWithRetry_PollsUntilTheWindowCloses holds sql/query to
// the common step fields of SDD section 6.3. Retry is the only repetition
// mechanism in the system and it is a field of the one step schema, so a
// sql/query that carries it must poll. A step that reads once forces the author
// back to a sleep, which the design bans.
func TestExecute_SQLQueryWithRetry_PollsUntilTheWindowCloses(t *testing.T) {
	handle, recorded := bindRecordingDatabase(t)
	rc := bindRunContext(handle)

	rows := 1
	step := &scenario.Step{
		Name:   "the outbox row appears",
		Type:   "sql/query",
		Target: "app-db",
		Query:  "SELECT id FROM outbox",
		Expect: &scenario.Expect{Rows: &rows},
		Retry:  &scenario.RetrySpec{Until: "pass", Timeout: "100ms", Interval: "10ms"},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pass {
		t.Fatalf("the step passed against a database that answered no rows")
	}
	if calls := recorded(); len(calls) < 2 {
		t.Errorf("the step ran %d statements over a 100ms window, want a poll", len(calls))
	}
	if result.Attempts < 2 {
		t.Errorf("attempts = %d, want the count of the poll", result.Attempts)
	}
}

// TestExecute_InboxAssertWithRetry_PollsUntilTheWindowCloses is the inbox/assert
// half of the same rule of SDD section 6.3. The consumer writes its inbox row
// after the broker delivers, so this is the step most likely to carry a retry.
func TestExecute_InboxAssertWithRetry_PollsUntilTheWindowCloses(t *testing.T) {
	handle, recorded := bindRecordingDatabase(t)
	rc := bindRunContext(handle)
	rc.Config.Targets.AppDB = &config.DatabaseTarget{
		Driver: "postgres",
		Inbox:  &config.InboxConfig{Table: "inbox", MessageIDColumn: "message_id"},
	}

	rows := 1
	step := &scenario.Step{
		Name:   "the redelivery was deduplicated",
		Type:   "inbox/assert",
		Target: "app-db",
		Where:  map[string]any{"messageId": "m-1"},
		Expect: &scenario.Expect{Rows: &rows},
		Retry:  &scenario.RetrySpec{Until: "pass", Timeout: "100ms", Interval: "10ms"},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pass {
		t.Fatalf("the step passed against an inbox that answered no rows")
	}
	if calls := recorded(); len(calls) < 2 {
		t.Errorf("the step ran %d statements over a 100ms window, want a poll", len(calls))
	}
	if result.Attempts < 2 {
		t.Errorf("attempts = %d, want the count of the poll", result.Attempts)
	}
}

// TestExecuteNotCaptured_UnparseableQuietPeriod_IsAnError holds the per-step
// quiet period of SDD section 8.3. A step that overrides the quiet period and
// writes a value the parser cannot read must fail. Falling back to the config
// default shortens the window the author asked for, and a message that arrives
// after the shorter window makes the assertion report a pass.
func TestExecuteNotCaptured_UnparseableQuietPeriod_IsAnError(t *testing.T) {
	srv, _ := captureQueryServer(t, "[]")

	rc := notCapturedContext(t, srv.URL, srv.Client())
	rc.Config.Defaults = awaitDefaults(t, "10s", "1ms")

	step := &scenario.Step{
		Name:        "no rejection event reached the broker",
		Type:        "event/not-captured",
		Exchange:    "public.orchestration-engine.Article.v1",
		QuietPeriod: "30 seconds",
	}

	_, err := executeNotCaptured(context.Background(), step, rc)
	if err == nil {
		t.Fatalf("an unreadable quiet period passed, so the step slept the config default instead")
	}
	if !strings.Contains(err.Error(), "30 seconds") {
		t.Errorf("the error is %q, want it to name the value the step wrote", err)
	}
}

// The recording driver below stands in for a database so that a step test can
// read the statement and the bound arguments without a container.

type boundStatement struct {
	query string
	args  []any
}

type bindLog struct {
	mu    sync.Mutex
	calls []boundStatement
}

func (l *bindLog) add(query string, args []driver.NamedValue) {
	l.mu.Lock()
	defer l.mu.Unlock()
	values := make([]any, len(args))
	for i, a := range args {
		values[i] = a.Value
	}
	l.calls = append(l.calls, boundStatement{query: query, args: values})
}

func (l *bindLog) snapshot() []boundStatement {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]boundStatement, len(l.calls))
	copy(out, l.calls)
	return out
}

var (
	bindLogs   = map[string]*bindLog{}
	bindLogsMu sync.Mutex
	bindSeq    int
)

type bindDriver struct{}

func (bindDriver) Open(name string) (driver.Conn, error) {
	bindLogsMu.Lock()
	defer bindLogsMu.Unlock()
	log, ok := bindLogs[name]
	if !ok {
		return nil, errors.New("no recording log for " + name)
	}
	return &bindConn{log: log}, nil
}

type bindConn struct{ log *bindLog }

func (c *bindConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("the recording driver takes a context call only")
}
func (c *bindConn) Close() error              { return nil }
func (c *bindConn) Begin() (driver.Tx, error) { return nil, errors.New("no transaction") }

func (c *bindConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.log.add(query, args)
	return &bindRows{}, nil
}

func (c *bindConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.log.add(query, args)
	return driver.RowsAffected(0), nil
}

type bindRows struct{}

func (r *bindRows) Columns() []string              { return []string{"id"} }
func (r *bindRows) Close() error                   { return nil }
func (r *bindRows) Next(dest []driver.Value) error { return io.EOF }

func init() {
	sql.Register("citadel-steps-binding", bindDriver{})
}

// bindRecordingDatabase opens a database handle backed by the recording driver and
// returns a reader over the statements it received.
func bindRecordingDatabase(t *testing.T) (*sql.DB, func() []boundStatement) {
	t.Helper()
	bindLogsMu.Lock()
	bindSeq++
	name := "log-" + strconv.Itoa(bindSeq)
	log := &bindLog{}
	bindLogs[name] = log
	bindLogsMu.Unlock()

	handle, err := sql.Open("citadel-steps-binding", name)
	if err != nil {
		t.Fatalf("open the recording database: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return handle, log.snapshot
}

func bindRunContext(handle *sql.DB) *RunContext {
	return &RunContext{
		Config:     &config.CitadelConfig{},
		RunID:      "run-1",
		RunStart:   time.Now(),
		HTTPClient: &http.Client{},
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
		DBs:        map[string]*sql.DB{"app-db": handle},
	}
}
