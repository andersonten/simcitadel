package runner

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// hitRecorder counts the requests that each preflight target received.
type hitRecorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func newHitRecorder() *hitRecorder {
	return &hitRecorder{counts: make(map[string]int)}
}

func (h *hitRecorder) record(target string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[target]++
}

func (h *hitRecorder) count(target string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[target]
}

func recordingServer(t *testing.T, rec *hitRecorder, target string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(target)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wiremockServer(t *testing.T, rec *hitRecorder, status int) *httptest.Server {
	t.Helper()
	return recordingServer(t, rec, "wiremock", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})
}

func captureServer(t *testing.T, rec *hitRecorder, statuses map[string]string) *httptest.Server {
	t.Helper()
	return recordingServer(t, rec, "event-capture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "exchanges": statuses})
	})
}

func triggerServer(t *testing.T, rec *hitRecorder) *httptest.Server {
	t.Helper()
	return recordingServer(t, rec, "event-trigger", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"broker":"connected"}`))
	})
}

func appServer(t *testing.T, rec *hitRecorder) *httptest.Server {
	t.Helper()
	return recordingServer(t, rec, "app", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func rabbitServer(t *testing.T, rec *hitRecorder, lockDeclared bool) *httptest.Server {
	t.Helper()
	return recordingServer(t, rec, "rabbitmq", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "citadel.lock") {
			if !lockDeclared {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"name":"citadel.lock","messages":0}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
}

func debeziumServer(t *testing.T, rec *hitRecorder, health, connectorState string) *httptest.Server {
	t.Helper()
	return recordingServer(t, rec, "debezium", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": health,
			"checks": []any{
				map[string]any{
					"name":   "debezium",
					"status": health,
					"data":   map[string]any{"state": connectorState},
				},
			},
		})
	})
}

// fakeDatabase records the statements that the preflight runs, so a test can
// prove the SELECT 1 of SDD section 4.10 without a database server.
type fakeDatabase struct {
	mu       sync.Mutex
	queries  []string
	failWith error
}

func (f *fakeDatabase) Open(string) (driver.Conn, error) {
	return &fakeConn{db: f}, nil
}

func (f *fakeDatabase) record(query string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, query)
}

func (f *fakeDatabase) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func (f *fakeDatabase) queried(query string) bool {
	for _, q := range f.recorded() {
		if q == query {
			return true
		}
	}
	return false
}

type fakeConn struct{ db *fakeDatabase }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	c.db.record(query)
	if c.db.failWith != nil {
		return nil, c.db.failWith
	}
	return &fakeStmt{}, nil
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake database has no transactions")
}

type fakeStmt struct{}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return 0 }

func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("fake database has no exec")
}

func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) { return &fakeRows{}, nil }

type fakeRows struct{ served bool }

func (r *fakeRows) Columns() []string { return []string{"one"} }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.served {
		return io.EOF
	}
	r.served = true
	dest[0] = int64(1)
	return nil
}

// fakeDriverSeq keeps every registered driver name unique, because the sql
// package rejects a second registration of one name.
var fakeDriverSeq atomic.Int64

func openFakeDatabase(t *testing.T, failWith error) (*sql.DB, *fakeDatabase) {
	t.Helper()
	fake := &fakeDatabase{failWith: failWith}
	name := fmt.Sprintf("citadel-fake-%d", fakeDriverSeq.Add(1))
	sql.Register(name, fake)

	handle, err := sql.Open(name, "fake")
	if err != nil {
		t.Fatalf("open the fake database: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return handle, fake
}

// appOnlyScenario references the app alone. It names no WireMock scenario, no
// database and no captured message, so preflight must leave those targets
// alone.
func appOnlyScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Name: "app-only",
		Steps: []scenario.Step{
			{Name: "read the app", Type: "app/http", HTTP: &scenario.HTTPSource{Path: "/ok"}},
		},
	}
}

func wireMockScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Name: "wiremock-state",
		Steps: []scenario.Step{
			{Name: "set the state", Type: "wiremock/set-state", ScenarioName: "articles", State: "submitted"},
		},
	}
}

// TestPreflight_ScenarioWithoutWireMockOrCDC_DoesNotContactThem holds SDD
// section 4.10 to its opening rule: only the targets the scenario references
// are checked, so a broken Debezium does not block a scenario without CDC.
func TestPreflight_ScenarioWithoutWireMockOrCDC_DoesNotContactThem(t *testing.T) {
	rec := newHitRecorder()
	wiremock := wiremockServer(t, rec, http.StatusOK)
	capture := captureServer(t, rec, map[string]string{"public.a.v1": "bound"})
	trigger := triggerServer(t, rec)
	rabbit := rabbitServer(t, rec, true)
	app := appServer(t, rec)
	debezium := debeziumServer(t, rec, "UP", "RUNNING")

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets: config.TargetsConfig{
			WireMock:       &config.WireMockTarget{URL: wiremock.URL, AdminURL: wiremock.URL + "/__admin"},
			EventCapture:   &config.EventCaptureTarget{URL: capture.URL},
			EventTrigger:   &config.EventTriggerTarget{URL: trigger.URL},
			RabbitMQ:       &config.RabbitMQTarget{AMQPURL: "amqp://127.0.0.1:5672/", ManagementURL: rabbit.URL},
			App:            &config.AppTarget{URL: app.URL},
			DebeziumOutbox: &config.DebeziumOutboxTarget{URL: debezium.URL, ConnectorName: "outbox"},
		},
		Capture: config.CaptureConfig{Exchanges: []config.CaptureExchange{{Name: "public.a.v1"}}},
	}

	scenarios := []*scenario.Scenario{appOnlyScenario()}

	if err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, nil); err != nil {
		t.Fatalf("preflight for an app-only scenario: %v", err)
	}

	for _, target := range []string{"wiremock", "event-capture", "event-trigger", "debezium"} {
		if got := rec.count(target); got != 0 {
			t.Errorf("preflight contacted %s %d time(s), but the scenario does not reference it", target, got)
		}
	}
	if rec.count("app") == 0 {
		t.Error("preflight did not contact the app, which the scenario references")
	}
	if rec.count("rabbitmq") == 0 {
		t.Error("preflight did not contact RabbitMQ, which every run references")
	}
}

// TestPreflight_ScenarioReferencesWireMock_FailsWhenWireMockIsDown proves the
// other half of the rule: a referenced target that is unhealthy stops the run
// with an environment failure.
func TestPreflight_ScenarioReferencesWireMock_FailsWhenWireMockIsDown(t *testing.T) {
	rec := newHitRecorder()
	wiremock := wiremockServer(t, rec, http.StatusInternalServerError)
	rabbit := rabbitServer(t, rec, true)

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{URL: wiremock.URL, AdminURL: wiremock.URL + "/__admin"},
			RabbitMQ: &config.RabbitMQTarget{AMQPURL: "amqp://127.0.0.1:5672/", ManagementURL: rabbit.URL},
		},
	}

	scenarios := []*scenario.Scenario{wireMockScenario()}

	err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, nil)
	if err == nil {
		t.Fatal("preflight passed with an unhealthy WireMock that the scenario references")
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d (error: %v)", got, ExitEnvironment, err)
	}
}

// TestPreflight_SpyQueueNotBound_FailsNamingTheExchange holds SDD sections 4.10
// and 8.1. Preflight treats anything but "bound" as not ready, because an
// unbound spy queue fails an outbound assertion for a reason that has nothing
// to do with the app.
func TestPreflight_SpyQueueNotBound_FailsNamingTheExchange(t *testing.T) {
	rec := newHitRecorder()
	capture := captureServer(t, rec, map[string]string{
		"public.a.v1": "bound",
		"public.b.v1": "pending",
	})
	rabbit := rabbitServer(t, rec, true)

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets: config.TargetsConfig{
			EventCapture: &config.EventCaptureTarget{URL: capture.URL},
			RabbitMQ:     &config.RabbitMQTarget{AMQPURL: "amqp://127.0.0.1:5672/", ManagementURL: rabbit.URL},
		},
		Capture: config.CaptureConfig{Exchanges: []config.CaptureExchange{
			{Name: "public.a.v1"},
			{Name: "public.b.v1"},
		}},
	}

	sc := &scenario.Scenario{
		Name: "captured",
		Steps: []scenario.Step{
			{Name: "the event arrives", Type: "event/captured", Exchange: "public.b.v1"},
		},
	}
	scenarios := []*scenario.Scenario{sc}

	err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, nil)
	if err == nil {
		t.Fatal("preflight passed while a configured spy queue reports pending")
	}
	if !strings.Contains(err.Error(), "public.b.v1") {
		t.Errorf("error does not name the unbound exchange: %v", err)
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d (error: %v)", got, ExitEnvironment, err)
	}
}

// sqlScenarioConfig builds a configuration with one database target and the
// broker that every run needs.
func sqlScenarioConfig(rabbitURL string) *config.CitadelConfig {
	return &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets: config.TargetsConfig{
			RabbitMQ: &config.RabbitMQTarget{AMQPURL: "amqp://127.0.0.1:5672/", ManagementURL: rabbitURL},
			AppDB:    &config.DatabaseTarget{Driver: "postgres", DSN: "postgres://127.0.0.1:5432/app"},
		},
	}
}

func sqlScenario() *scenario.Scenario {
	return &scenario.Scenario{
		Name: "sql",
		Steps: []scenario.Step{
			{Name: "seed the row", Type: "sql/exec", Target: "app-db", Query: "insert into t values (1)"},
		},
	}
}

// TestPreflight_DatabaseTargetReferenced_RunsSelectOne holds the app-db and
// upstream-shadow row of SDD section 4.10.
func TestPreflight_DatabaseTargetReferenced_RunsSelectOne(t *testing.T) {
	rec := newHitRecorder()
	rabbit := rabbitServer(t, rec, true)
	cfg := sqlScenarioConfig(rabbit.URL)
	handle, fake := openFakeDatabase(t, nil)

	scenarios := []*scenario.Scenario{sqlScenario()}
	databases := map[string]*sql.DB{"app-db": handle}

	if err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, databases); err != nil {
		t.Fatalf("preflight with a database that answers: %v", err)
	}
	if !fake.queried("SELECT 1") {
		t.Errorf("preflight ran %v against app-db, want SELECT 1", fake.recorded())
	}
}

// TestPreflight_DatabaseTargetDoesNotAnswer_FailsWithEnvironmentError proves the
// SELECT 1 result decides the run, rather than the connection alone.
func TestPreflight_DatabaseTargetDoesNotAnswer_FailsWithEnvironmentError(t *testing.T) {
	rec := newHitRecorder()
	rabbit := rabbitServer(t, rec, true)
	cfg := sqlScenarioConfig(rabbit.URL)
	handle, _ := openFakeDatabase(t, errors.New("connection reset by peer"))

	scenarios := []*scenario.Scenario{sqlScenario()}
	databases := map[string]*sql.DB{"app-db": handle}

	err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, databases)
	if err == nil {
		t.Fatal("preflight passed while SELECT 1 fails on the referenced database target")
	}
	if !strings.Contains(err.Error(), "app-db") {
		t.Errorf("error does not name the database target: %v", err)
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d (error: %v)", got, ExitEnvironment, err)
	}
}

// TestPreflight_UnreferencedDatabaseTarget_IsNotChecked keeps a configured
// database that no scenario touches out of the way of the run.
func TestPreflight_UnreferencedDatabaseTarget_IsNotChecked(t *testing.T) {
	rec := newHitRecorder()
	rabbit := rabbitServer(t, rec, true)
	cfg := sqlScenarioConfig(rabbit.URL)
	cfg.Targets.UpstreamShadow = &config.DatabaseTarget{Driver: "postgres", DSN: "postgres://127.0.0.1:5432/shadow"}
	handle, _ := openFakeDatabase(t, nil)
	broken, _ := openFakeDatabase(t, errors.New("shadow is down"))

	scenarios := []*scenario.Scenario{sqlScenario()}
	databases := map[string]*sql.DB{"app-db": handle, "upstream-shadow": broken}

	if err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, databases); err != nil {
		t.Fatalf("preflight failed on a database target that the scenario does not reference: %v", err)
	}
}

// TestPreflight_DebeziumConnectorNotRunning_FailsEvenWhenHealthIsUP holds the
// last row of SDD section 4.10. Debezium reports healthy while a connector has
// failed, so health alone is not evidence.
func TestPreflight_DebeziumConnectorNotRunning_FailsEvenWhenHealthIsUP(t *testing.T) {
	rec := newHitRecorder()
	rabbit := rabbitServer(t, rec, true)
	capture := captureServer(t, rec, map[string]string{"public.a.v1": "bound"})
	debezium := debeziumServer(t, rec, "UP", "FAILED")

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets: config.TargetsConfig{
			RabbitMQ:       &config.RabbitMQTarget{AMQPURL: "amqp://127.0.0.1:5672/", ManagementURL: rabbit.URL},
			EventCapture:   &config.EventCaptureTarget{URL: capture.URL},
			DebeziumOutbox: &config.DebeziumOutboxTarget{URL: debezium.URL, ConnectorName: "outbox"},
		},
		Capture: config.CaptureConfig{Exchanges: []config.CaptureExchange{{Name: "public.a.v1"}}},
	}

	sc := &scenario.Scenario{
		Name: "cdc",
		Steps: []scenario.Step{
			{Name: "the outbox event arrives", Type: "event/captured", Exchange: "public.a.v1"},
		},
	}
	scenarios := []*scenario.Scenario{sc}

	err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, nil)
	if err == nil {
		t.Fatal("preflight passed while the Debezium connector state is FAILED")
	}
	if !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("error does not report the connector state: %v", err)
	}
}

const scenarioTwoAppCalls = `name: two-app-calls
steps:
  - name: first call
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
  - name: second call
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
`

// TestRun_TokenExpiresDuringTheRun_IsRefreshed holds SDD section 9, which
// caches the token in memory for the run and refreshes it 60 s before expiry.
// A token that lives one second must be replaced before the second step.
func TestRun_TokenExpiresDuringTheRun_IsRefreshed(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)

	var issued atomic.Int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := issued.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"token-%d","expires_in":1}`, n)
	}))
	t.Cleanup(tokenSrv.Close)

	cfg := env.config()
	cfg.Auth = map[string]config.AuthConfig{
		"hydra": {TokenURL: tokenSrv.URL, ClientID: "citadel", ClientSecret: "shhh"},
	}
	cfg.Targets.App.Auth = "hydra"

	dir := t.TempDir()
	path := writeScenario(t, dir, "auth.yaml", scenarioTwoAppCalls)

	code, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: cfg})
	if code != ExitSuccess {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitSuccess, dumpEvents(events))
	}
	if got := issued.Load(); got < 2 {
		t.Errorf("token endpoint served %d request(s), want at least 2, because the token expires during the run", got)
	}
}

// tokenTestSource builds a source against a token endpoint that numbers every
// token it issues, with a clock the test moves by hand.
func tokenTestSource(t *testing.T, expiresIn int, clock *time.Time) (*TokenSource, func() int64) {
	t.Helper()

	var issued atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := issued.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"token-%d","expires_in":%d}`, n, expiresIn)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets:     config.TargetsConfig{App: &config.AppTarget{URL: "http://app.invalid", Auth: "hydra"}},
		Auth: map[string]config.AuthConfig{
			"hydra": {TokenURL: srv.URL, ClientID: "citadel", ClientSecret: "shhh"},
		},
	}

	source := &TokenSource{cfg: cfg, client: srv.Client(), now: func() time.Time { return *clock }}
	return source, issued.Load
}

// TestTokenSource_TokenInsideTheRefreshMargin_IsReplaced holds SDD section 9,
// which refreshes the token 60 s before it expires.
func TestTokenSource_TokenInsideTheRefreshMargin_IsReplaced(t *testing.T) {
	clock := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	source, issued := tokenTestSource(t, 300, &clock)

	first, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if first != "token-1" {
		t.Fatalf("first token = %q, want %q", first, "token-1")
	}

	// 250 s in, the token has 50 s of life left, which is inside the margin.
	clock = clock.Add(250 * time.Second)
	second, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if second != "token-2" {
		t.Errorf("token = %q, want a refreshed %q", second, "token-2")
	}
	if got := issued(); got != 2 {
		t.Errorf("token endpoint served %d request(s), want 2", got)
	}
}

// TestTokenSource_TokenOutsideTheRefreshMargin_StaysCached holds the other half
// of SDD section 9: the token is cached in memory for the run.
func TestTokenSource_TokenOutsideTheRefreshMargin_StaysCached(t *testing.T) {
	clock := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	source, issued := tokenTestSource(t, 3600, &clock)

	if _, err := source.Token(context.Background()); err != nil {
		t.Fatalf("first token: %v", err)
	}

	clock = clock.Add(10 * time.Minute)
	again, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	if again != "token-1" {
		t.Errorf("token = %q, want the cached %q", again, "token-1")
	}
	if got := issued(); got != 1 {
		t.Errorf("token endpoint served %d request(s), want 1", got)
	}
}

// TestPreflight_LockQueueMissing_FailsWithEnvironmentError holds the rabbitmq
// row of SDD section 4.10, which requires the lock queue to be declared.
func TestPreflight_LockQueueMissing_FailsWithEnvironmentError(t *testing.T) {
	rec := newHitRecorder()
	rabbit := rabbitServer(t, rec, false)

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets: config.TargetsConfig{
			RabbitMQ: &config.RabbitMQTarget{AMQPURL: "amqp://127.0.0.1:5672/", ManagementURL: rabbit.URL},
		},
	}

	scenarios := []*scenario.Scenario{appOnlyScenario()}

	err := Preflight(context.Background(), cfg, &http.Client{}, scenarios, nil)
	if err == nil {
		t.Fatal("preflight passed while the lock queue is not declared")
	}
	if !strings.Contains(err.Error(), "citadel.lock") {
		t.Errorf("error does not name the lock queue: %v", err)
	}
}
