package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the run lifecycle of SDD section 4.2. Each test states
// one behaviour the section requires, in the order the phases run.

type lifecycleEnv struct {
	srv            *httptest.Server
	wiremock       *httptest.Server
	scenarioResets atomic.Int64
	requestResets  atomic.Int64
	boomCalls      atomic.Int64
}

// newLifecycleEnv stands up one server that answers every admin, readiness and
// probe path the runner touches, and counts the reset calls.
func newLifecycleEnv(t *testing.T) *lifecycleEnv {
	t.Helper()
	env := &lifecycleEnv{}
	mux := http.NewServeMux()

	mux.HandleFunc("/__admin/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/__admin/scenarios/reset", func(w http.ResponseWriter, r *http.Request) {
		env.scenarioResets.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	// The journal is cleared with DELETE /__admin/requests. The method is part of
	// the pattern, so this stub answers only the request the pinned WireMock
	// serves, and a return to POST /__admin/requests/reset fails this test rather
	// than passing against a stub that accepts anything.
	mux.HandleFunc("DELETE /__admin/requests", func(w http.ResponseWriter, r *http.Request) {
		env.requestResets.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/__admin/requests/unmatched", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"requests":[]}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":"OK"}`))
	})
	// A probe target that fails expect.status 200, slowly enough that a real
	// duration is distinguishable from a hardcoded zero.
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		env.boomCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"value":"BOOM"}`))
	})
	// A target that never answers inside a step timeout, so the step errors
	// rather than missing an expectation.
	mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/queues", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	env.srv = httptest.NewServer(mux)
	t.Cleanup(env.srv.Close)
	// WireMock listens on its own address. SDD section 13 check 6 forbids a probe against the
	// mock, and check 6 of SDD section 13 enforces it by comparing addresses.
	env.wiremock = httptest.NewServer(mux)
	t.Cleanup(env.wiremock.Close)
	return env
}

func (e *lifecycleEnv) config() *config.CitadelConfig {
	return &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "verify"},
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{URL: e.wiremock.URL, AdminURL: e.wiremock.URL + "/__admin"},
			App:      &config.AppTarget{URL: e.srv.URL},
			RabbitMQ: &config.RabbitMQTarget{
				AMQPURL:       "amqp://guest:guest@127.0.0.1:5672/",
				ManagementURL: e.srv.URL,
			},
		},
	}
}

// stubLock replaces the broker lock for the duration of one test. The lock
// itself is covered against a real broker; these tests cover the lifecycle
// around it.
func stubLock(t *testing.T) {
	t.Helper()
	orig := acquireLock
	acquireLock = func(ctx context.Context, amqpURL, environment string) (*Lock, error) {
		return &Lock{}, nil
	}
	t.Cleanup(func() { acquireLock = orig })
}

// captureLogs redirects slog to a buffer and returns it.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

func writeScenario(t *testing.T, dir, file, body string) string {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write scenario %s: %v", path, err)
	}
	return path
}

// runCapture executes Run with stdout redirected, and returns the exit code
// with the parsed newline-delimited JSON events.
func runCapture(t *testing.T, rc RunConfig) (int, []map[string]any) {
	t.Helper()
	rc.Output = "json"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 0, 64*1024)
		chunk := make([]byte, 4096)
		for {
			n, readErr := r.Read(chunk)
			if n > 0 {
				buf = append(buf, chunk[:n]...)
			}
			if readErr != nil {
				break
			}
		}
		done <- buf
	}()

	code := Run(context.Background(), rc)

	os.Stdout = orig
	_ = w.Close()
	raw := <-done
	_ = r.Close()

	var events []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("event line is not JSON: %q (%v)", line, err)
		}
		events = append(events, evt)
	}
	return code, events
}

func eventsOfType(events []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func dumpEvents(events []map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		raw, _ := json.Marshal(e)
		b.WriteString("      " + string(raw) + "\n")
	}
	return b.String()
}

const scenarioThreeFailingAssertions = `name: three-failing-assertions
steps:
  - name: a passing step
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
assertions:
  - name: assertion one
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
  - name: assertion two
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
  - name: assertion three
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
`

const scenarioFailingStepThenAssertions = `name: failing-step-then-assertions
steps:
  - name: the step that fails
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
  - name: the step that must not run
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
assertions:
  - name: assertion one
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
  - name: assertion two
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
  - name: assertion three
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
`

const scenarioAllPass = `name: all-pass
steps:
  - name: a passing step
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
`

// TestRun_ThreeFailingAssertions_AllRunAndReport holds SDD section 4.2 to its
// decision that assertions always all run. A scenario that fails three
// assertions must report all three, with --keep-going set or not.
func TestRun_ThreeFailingAssertions_AllRunAndReport(t *testing.T) {
	for _, keepGoing := range []bool{false, true} {
		stubLock(t)
		env := newLifecycleEnv(t)
		dir := t.TempDir()
		path := writeScenario(t, dir, "a.yaml", scenarioThreeFailingAssertions)

		code, events := runCapture(t, RunConfig{
			ScenarioPaths: []string{path},
			Config:        env.config(),
			KeepGoing:     keepGoing,
		})

		fails := eventsOfType(events, "step.fail")
		if len(fails) != 3 {
			t.Errorf("keepGoing=%v: step.fail count = %d, want 3\n%s",
				keepGoing, len(fails), dumpEvents(events))
		}
		if code != ExitFailure {
			t.Errorf("keepGoing=%v: exit = %d, want %d", keepGoing, code, ExitFailure)
		}
	}
}

// TestRun_FailedStep_StopsStepsButRunsEveryAssertion holds the other half of
// SDD section 4.2: a failed step aborts the remaining steps only. Phase 9 still
// runs in full.
func TestRun_FailedStep_StopsStepsButRunsEveryAssertion(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioFailingStepThenAssertions)

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{path},
		Config:        env.config(),
	})

	var started []string
	for _, s := range eventsOfType(events, "step.start") {
		started = append(started, fmt.Sprintf("%v", s["name"])) //nolint:perfsprint
	}

	// The failing step plus all three assertions. The second step must not run.
	want := []string{"the step that fails", "assertion one", "assertion two", "assertion three"}
	if strings.Join(started, "|") != strings.Join(want, "|") {
		t.Errorf("steps started = %v, want %v\n%s", started, want, dumpEvents(events))
	}
	if code != ExitFailure {
		t.Errorf("exit = %d, want %d", code, ExitFailure)
	}
}

// TestRun_TwoScenarios_ResetRunsBeforeEachOne holds SDD section 4.9: reset is
// unconditional and precedes every scenario, so scenario b never inherits
// scenario a's WireMock state or request journal.
func TestRun_TwoScenarios_ResetRunsBeforeEachOne(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	a := writeScenario(t, dir, "a.yaml", scenarioAllPass)
	b := writeScenario(t, dir, "b.yaml", scenarioAllPass)

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{a, b},
		Config:        env.config(),
	})

	if code != ExitSuccess {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitSuccess, dumpEvents(events))
	}
	if got := env.scenarioResets.Load(); got != 2 {
		t.Errorf("POST /__admin/scenarios/reset count = %d, want 2 (one per scenario)", got)
	}
	if got := env.requestResets.Load(); got != 2 {
		t.Errorf("DELETE /__admin/requests count = %d, want 2 (one per scenario)", got)
	}
}

// TestRun_TwoScenarios_TruncateListsStaySeparate proves the reset before
// scenario a carries scenario a's truncate list alone. Merging the lists would
// let the first scenario run against the last scenario's fixtures.
func TestRun_TwoScenarios_TruncateListsStaySeparate(t *testing.T) {
	stubLock(t)
	logs := captureLogs(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()

	a := writeScenario(t, dir, "a.yaml", `name: trunc-a
reset:
  truncate:
    - a_one
steps:
  - name: a passing step
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
`)
	b := writeScenario(t, dir, "b.yaml", `name: trunc-b
reset:
  truncate:
    - b_one
    - b_two
steps:
  - name: a passing step
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
`)

	// With no app-db connected, reset refuses and names the table count it was
	// given. That count is the observable proof of which list arrived.
	code, _ := runCapture(t, RunConfig{ScenarioPaths: []string{a, b}, Config: env.config()})
	if code != ExitEnvironment {
		t.Fatalf("exit = %d, want %d", code, ExitEnvironment)
	}

	out := logs.String()
	if !strings.Contains(out, "1 table(s)") {
		t.Errorf("reset guard did not report scenario a's single table; logs:\n%s", out)
	}
	if strings.Contains(out, "3 table(s)") {
		t.Errorf("reset received the merged list of both scenarios; logs:\n%s", out)
	}
}

// TestRun_WithoutAMQPURL_FailsBecauseTheLockIsUnconditional holds SDD section
// 4.2 phase 3 and section 4.8. A run with nowhere to put the lock must stop,
// not proceed unlocked.
func TestRun_WithoutAMQPURL_FailsBecauseTheLockIsUnconditional(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioAllPass)

	cases := []struct {
		name   string
		mutate func(*config.CitadelConfig)
	}{
		{
			name:   "rabbitmq target absent",
			mutate: func(c *config.CitadelConfig) { c.Targets.RabbitMQ = nil },
		},
		{
			name: "rabbitmq present with empty amqpUrl",
			mutate: func(c *config.CitadelConfig) {
				c.Targets.RabbitMQ = &config.RabbitMQTarget{AMQPURL: "", ManagementURL: env.srv.URL}
			},
		},
	}

	for _, tc := range cases {
		cfg := env.config()
		tc.mutate(cfg)
		code, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: cfg})
		if code != ExitInvalidInput {
			t.Errorf("%s: exit = %d, want %d\n%s", tc.name, code, ExitInvalidInput, dumpEvents(events))
		}
		if len(eventsOfType(events, "step.start")) != 0 {
			t.Errorf("%s: a step ran without the environment lock\n%s", tc.name, dumpEvents(events))
		}
	}
}

// TestRun_UnreachableBroker_ExitsEnvironment confirms the lock path is reached
// and reported, rather than skipped, when the broker is configured but down.
func TestRun_UnreachableBroker_ExitsEnvironment(t *testing.T) {
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioAllPass)

	cfg := env.config()
	cfg.Targets.RabbitMQ = &config.RabbitMQTarget{
		AMQPURL:       "amqp://guest:guest@127.0.0.1:1/",
		ManagementURL: env.srv.URL,
	}

	code, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: cfg})
	if code != ExitEnvironment {
		t.Errorf("exit = %d, want %d\n%s", code, ExitEnvironment, dumpEvents(events))
	}
}

// TestRun_StepThatErrors_ReportsARealDuration holds the step.fail schema of SDD
// section 4.4. A step that spends its timeout and then errors must not report
// durationMs 0, because that hides where the run spent its time.
func TestRun_StepThatErrors_ReportsARealDuration(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()

	// The step spends its whole timeout against a target that never answers,
	// then errors. A hardcoded 0 would hide those 300 milliseconds.
	path := writeScenario(t, dir, "err.yaml", `name: step-returns-error
steps:
  - name: step that errors after a real delay
    type: app/http
    timeout: 300ms
    http:
      path: /hang
    expect:
      status: 200
`)

	_, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: env.config()})

	fails := eventsOfType(events, "step.fail")
	if len(fails) != 1 {
		t.Fatalf("step.fail count = %d, want 1\n%s", len(fails), dumpEvents(events))
	}
	dur, ok := fails[0]["durationMs"].(float64)
	if !ok {
		t.Fatalf("step.fail has no numeric durationMs: %v", fails[0])
	}
	if dur <= 0 {
		t.Errorf("durationMs = %v, want the real elapsed time\n%s", dur, dumpEvents(events))
	}
}

// TestRun_TwoScenarios_EmitsOneRunStartAndOneRunEnd holds SDD section 4.4. The
// events are named run.start and run.end, so a run emits exactly one of each,
// however many scenarios it contains.
func TestRun_TwoScenarios_EmitsOneRunStartAndOneRunEnd(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	a := writeScenario(t, dir, "a.yaml", scenarioAllPass)
	b := writeScenario(t, dir, "b.yaml", scenarioAllPass)

	code, events := runCapture(t, RunConfig{ScenarioPaths: []string{a, b}, Config: env.config()})

	starts := eventsOfType(events, "run.start")
	ends := eventsOfType(events, "run.end")
	if len(starts) != 1 {
		t.Errorf("run.start count = %d, want 1\n%s", len(starts), dumpEvents(events))
	}
	if len(ends) != 1 {
		t.Errorf("run.end count = %d, want 1\n%s", len(ends), dumpEvents(events))
	}
	if len(starts) == 1 && starts[0]["schemaVersion"] == nil {
		t.Error("run.start has no schemaVersion")
	}
	if len(ends) == 1 && ends[0]["outcome"] != "pass" {
		t.Errorf("run.end outcome = %v, want \"pass\"", ends[0]["outcome"])
	}
	if code != ExitSuccess {
		t.Errorf("exit = %d, want %d", code, ExitSuccess)
	}
}

// TestEmitter_Timestamps_CarryMillisecondPrecision holds the sample stream of
// SDD section 4.4, where every ts has three fractional digits.
func TestEmitter_Timestamps_CarryMillisecondPrecision(t *testing.T) {
	buf := &bytes.Buffer{}
	NewEmitter(buf, "json", "run-1").RunStart("s", "local")

	var evt map[string]any
	if err := json.Unmarshal(buf.Bytes(), &evt); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	ts, _ := evt["ts"].(string)
	if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", ts); err != nil {
		t.Errorf("ts = %q, want millisecond precision like 2026-08-19T09:00:00.000Z", ts)
	}
}

// TestEmitter_RunEnd_UsesTheBundleKey holds the run.end sample of SDD section
// 4.4, which names the field bundle.
func TestEmitter_RunEnd_UsesTheBundleKey(t *testing.T) {
	buf := &bytes.Buffer{}
	NewEmitter(buf, "json", "run-1").RunEnd("fail", ExitFailure, ".citadel/runs/0191/")

	var evt map[string]any
	if err := json.Unmarshal(buf.Bytes(), &evt); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if evt["bundle"] != ".citadel/runs/0191/" {
		t.Errorf("run.end bundle = %v, want the bundle directory path", evt["bundle"])
	}
	if _, unexpected := evt["bundlePath"]; unexpected {
		t.Error("run.end carries bundlePath, but SDD 4.4 names the field bundle")
	}
}

const scenarioManyErrors = `name: many-errors
steps:
  - name: bad one
    type: totally/unknown
  - name: bad two
    type: event/publish
  - name: bad three
    type: probe
`

// TestLoadAndValidate_ManyBadScenarios_ReportsEveryError holds SDD section 4.2
// phase 2, which validates every scenario in the run. A developer must not pay
// one round trip per error.
func TestLoadAndValidate_ManyBadScenarios_ReportsEveryError(t *testing.T) {
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	a := writeScenario(t, dir, "a.yaml", scenarioManyErrors)
	b := writeScenario(t, dir, "b.yaml", scenarioManyErrors)

	sc, parseErr := scenario.Parse(a)
	if parseErr != nil {
		t.Fatalf("parse: %v", parseErr)
	}
	all := scenario.Validate(sc, dir)
	if len(all) < 2 {
		t.Fatalf("fixture produced %d validation errors, need at least 2", len(all))
	}

	_, err := loadAndValidate(RunConfig{ScenarioPaths: []string{a, b}, Config: env.config()})
	if err == nil {
		t.Fatal("loadAndValidate returned no error for two invalid scenarios")
	}

	for _, e := range all {
		if !strings.Contains(err.Error(), e.Error()) {
			t.Errorf("report omits %q\nreport:\n%s", e.Error(), err.Error())
		}
	}
	if !strings.Contains(err.Error(), "b.yaml") {
		t.Errorf("report omits the second file\nreport:\n%s", err.Error())
	}
	if ClassifyError(err) != ExitInvalidInput {
		t.Errorf("exit = %d, want %d", ClassifyError(err), ExitInvalidInput)
	}
}

// TestLoadAndValidate_DirectoryPath_ExpandsToEveryYAML holds the command
// surface of SDD section 4.1: citadel run takes a scenario file or a directory.
func TestLoadAndValidate_DirectoryPath_ExpandsToEveryYAML(t *testing.T) {
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	writeScenario(t, dir, "b.yaml", scenarioAllPass)
	writeScenario(t, dir, "a.yaml", strings.Replace(scenarioAllPass, "all-pass", "all-pass-a", 1))
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o600); err != nil {
		t.Fatalf("write non-scenario file: %v", err)
	}

	scenarios, err := loadAndValidate(RunConfig{ScenarioPaths: []string{dir}, Config: env.config()})
	if err != nil {
		t.Fatalf("loadAndValidate with a directory: %v", err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("loaded %d scenarios from the directory, want 2", len(scenarios))
	}
	// Lexical order keeps a run reproducible.
	if scenarios[0].Name != "all-pass-a" || scenarios[1].Name != "all-pass" {
		t.Errorf("scenario order = %s,%s, want all-pass-a,all-pass", scenarios[0].Name, scenarios[1].Name)
	}
}

// TestLoadAndValidate_EmptyDirectory_IsInvalidInput keeps an empty directory
// from silently producing a run with no scenario.
func TestLoadAndValidate_EmptyDirectory_IsInvalidInput(t *testing.T) {
	env := newLifecycleEnv(t)
	_, err := loadAndValidate(RunConfig{ScenarioPaths: []string{t.TempDir()}, Config: env.config()})
	if err == nil {
		t.Fatal("an empty directory produced no error")
	}
	if ClassifyError(err) != ExitInvalidInput {
		t.Errorf("exit = %d, want %d", ClassifyError(err), ExitInvalidInput)
	}
}
