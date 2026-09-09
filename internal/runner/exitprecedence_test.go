package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// This file holds the exit code precedence of SDD section 4.4 and the bundle
// write of section 12.1, including the paths a panic and a preflight failure
// take.

// panicLock replaces the lock seam with one that panics, which stands for a bug
// in Citadel in any phase before phase 9 of SDD section 4.2.
func panicLock(t *testing.T) {
	t.Helper()
	orig := acquireLock
	acquireLock = func(context.Context, string, string) (*Lock, error) {
		panic("a bug in Citadel")
	}
	t.Cleanup(func() { acquireLock = orig })
}

// bundleRunDirs lists the run directories a bundle root holds.
func bundleRunDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	return dirs
}

// TestRun_PanicOutsideTheScenario_ExitsInternalAndWritesTheBundle holds SDD
// section 4.2 phase 12, which runs even on panic, and section 4.4, which gives
// a bug in Citadel exit 6. The recover of the runner covers the scenario alone,
// so a panic in any other phase ends the process with neither the exit code nor
// the bundle.
func TestRun_PanicOutsideTheScenario_ExitsInternalAndWritesTheBundle(t *testing.T) {
	panicLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioAllPass)
	bundleRoot := filepath.Join(t.TempDir(), "runs")

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{path},
		Config:        env.config(),
		BundleDir:     bundleRoot,
	})

	if code != ExitInternal {
		t.Errorf("exit = %d, want %d\n%s", code, ExitInternal, dumpEvents(events))
	}
	if len(eventsOfType(events, "run.end")) != 1 {
		t.Errorf("run.end count = %d, want 1\n%s", len(eventsOfType(events, "run.end")), dumpEvents(events))
	}
	if dirs := bundleRunDirs(t, bundleRoot); len(dirs) != 1 {
		t.Errorf("bundle directory count = %d, want 1; SDD 4.2 phase 12 never skips the bundle write", len(dirs))
	}
}

// appFindingServer answers a wrong status on /boom and drops the connection on
// /dead, so one scenario can hold a finding about the application and a target
// it cannot reach.
func appFindingServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"value":"BOOM"}`))
	})
	mux.HandleFunc("/dead", func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const scenarioFindingThenEnvironment = `name: finding-then-environment
steps:
  - name: the application answers with the wrong status
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
assertions:
  - name: an assertion whose target drops the connection
    type: app/http
    http:
      path: /dead
    expect:
      status: 200
`

const scenarioEnvironmentThenFinding = `name: environment-then-finding
steps:
  - name: a step whose target drops the connection
    type: app/http
    optional: true
    http:
      path: /dead
    expect:
      status: 200
assertions:
  - name: the application answers with the wrong status
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
`

// TestRun_ApplicationFindingAndEnvironmentFailure_ExitsOneWhateverTheOrder holds
// SDD section 4.4. Exit 1 is "the only code that indicates a finding about the
// app", so a run that holds one must not report exit 3 and send the finding to
// the team that owns the environment. The runner already applies that rule to
// the end-of-run checks of section 4.11, and the last failure decided the code
// everywhere else, so the answer depended on the order of the assertions.
func TestRun_ApplicationFindingAndEnvironmentFailure_ExitsOneWhateverTheOrder(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "the finding comes first", body: scenarioFindingThenEnvironment},
		{name: "the environment failure comes first", body: scenarioEnvironmentThenFinding},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubLock(t)
			env := newLifecycleEnv(t)
			app := appFindingServer(t)
			dir := t.TempDir()
			path := writeScenario(t, dir, "a.yaml", tc.body)

			cfg := env.config()
			cfg.Targets.App = &config.AppTarget{URL: app.URL}

			code, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: cfg})

			var classes []string
			for _, evt := range eventsOfType(events, "step.fail") {
				failure, _ := evt["failure"].(map[string]any)
				classes = append(classes, strings.TrimSpace(failureClassOf(failure)))
			}
			if len(classes) != 2 {
				t.Fatalf("step.fail count = %d, want 2\n%s", len(classes), dumpEvents(events))
			}
			if !contains(classes, "assertion") || !contains(classes, "environment") {
				t.Fatalf("failure classes = %v, want one assertion and one environment\n%s",
					classes, dumpEvents(events))
			}
			if code != ExitFailure {
				t.Errorf("exit = %d, want %d; the finding about the application must not be reported as an environment problem\n%s",
					code, ExitFailure, dumpEvents(events))
			}
		})
	}
}

func failureClassOf(failure map[string]any) string {
	if failure == nil {
		return ""
	}
	class, _ := failure["class"].(string)
	return class
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestRun_WithoutManagementURL_DoesNotReportAPass holds SDD sections 4.9 and
// 4.11. Step 3 of the reset purges every queue through the management API, step
// 7 verifies the depths, and the unroutable check reads the same API. Without
// managementUrl all four go missing without a word, so a run whose message
// reached the unroutable queue reports a pass.
func TestRun_WithoutManagementURL_DoesNotReportAPass(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioAllPass)

	cfg := env.config()
	cfg.Targets.RabbitMQ.ManagementURL = ""

	code, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: cfg})

	if code != ExitInvalidInput {
		t.Errorf("exit = %d, want %d; a run with no management URL cannot purge a queue and cannot read the unroutable depth\n%s",
			code, ExitInvalidInput, dumpEvents(events))
	}
	if len(eventsOfType(events, "step.start")) != 0 {
		t.Errorf("a step ran although the reset could not purge a queue\n%s", dumpEvents(events))
	}
}

// TestRun_PreflightFailure_WritesTheBundleAndEmitsRunEnd holds SDD section 12.1,
// which writes the bundle "on any non-zero exit", and phase 12 of section 4.2,
// which is marked "Always". A failure in phases 3 to 8 returned from the middle
// of the run and wrote neither the bundle nor a run.end event, so a consumer of
// --output json read an empty stream.
func TestRun_PreflightFailure_WritesTheBundleAndEmitsRunEnd(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioAllPass)
	bundleRoot := filepath.Join(t.TempDir(), "runs")

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	cfg := env.config()
	cfg.Targets.RabbitMQ.ManagementURL = broken.URL

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{path},
		Config:        cfg,
		BundleDir:     bundleRoot,
	})

	if code != ExitEnvironment {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitEnvironment, dumpEvents(events))
	}

	ends := eventsOfType(events, "run.end")
	if len(ends) != 1 {
		t.Errorf("run.end count = %d, want 1\n%s", len(ends), dumpEvents(events))
	}

	dirs := bundleRunDirs(t, bundleRoot)
	if len(dirs) != 1 {
		t.Fatalf("bundle directory count = %d, want 1 for a non-zero exit", len(dirs))
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "run.json")); err != nil {
		t.Errorf("the bundle holds no run.json: %v", err)
	}
}

// TestRun_ConfiguredOutboxAndNoSQLStep_DoesNotReportAPass holds the
// skipped-outbox check of SDD section 4.11, which runs after the assertions and
// fails the run on any UPDATE or DELETE against the outbox table. The runner
// opened only the database targets a step names, so a scenario that writes no
// SQL left the check with no connection and the check reported nothing.
func TestRun_ConfiguredOutboxAndNoSQLStep_DoesNotReportAPass(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioAllPass)

	cfg := env.config()
	cfg.Targets.DebeziumOutbox = &config.DebeziumOutboxTarget{
		URL:           "http://127.0.0.1:1",
		ConnectorName: "reference-app-outbox",
	}
	// The port is closed, so the connection attempt ends at once. A run that
	// reports a pass here proves the check never looked at the outbox table.
	cfg.Targets.AppDB = &config.DatabaseTarget{
		Driver: "postgres",
		DSN:    "postgres://citadel@127.0.0.1:1/app",
	}

	code, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: cfg})

	if code == ExitSuccess {
		t.Errorf("exit = %d; the skipped-outbox check of SDD 4.11 read nothing and the run still reported a pass\n%s",
			code, dumpEvents(events))
	}
	if code != ExitEnvironment {
		t.Errorf("exit = %d, want %d naming the application database\n%s", code, ExitEnvironment, dumpEvents(events))
	}
}
