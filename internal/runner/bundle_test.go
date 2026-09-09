package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
)

const scenarioOneStepOneFailingAssertion = `name: bundle-scenario
variables:
  articleId: "515150"
steps:
  - name: a passing step
    type: app/http
    http:
      path: /ok
    expect:
      status: 200
assertions:
  - name: the failing assertion
    type: app/http
    http:
      path: /boom
    expect:
      status: 200
`

// runIDFromEvents reads the run identity that the emitter stamped on every line.
func runIDFromEvents(t *testing.T, events []map[string]any) string {
	t.Helper()
	for _, e := range events {
		if id, ok := e["runId"].(string); ok && id != "" {
			return id
		}
	}
	t.Fatalf("no runId in the event stream:\n%s", dumpEvents(events))
	return ""
}

// TestRun_FailedRun_BundleHoldsScenarioYAMLAndStepsJSONL holds SDD section
// 12.1. The bundle carries the scenario as loaded and one record per step,
// because a bundle without them cannot answer what the run actually did.
func TestRun_FailedRun_BundleHoldsScenarioYAMLAndStepsJSONL(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	bundleDir := filepath.Join(t.TempDir(), "runs")
	path := writeScenario(t, dir, "bundle.yaml", scenarioOneStepOneFailingAssertion)

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{path},
		Config:        env.config(),
		BundleDir:     bundleDir,
	})
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitFailure, dumpEvents(events))
	}

	runDir := filepath.Join(bundleDir, runIDFromEvents(t, events))

	scenarioYAML, err := os.ReadFile(filepath.Join(runDir, "scenario.yaml"))
	if err != nil {
		t.Fatalf("read scenario.yaml: %v", err)
	}
	for _, want := range []string{"bundle-scenario", "a passing step", "the failing assertion", "515150"} {
		if !strings.Contains(string(scenarioYAML), want) {
			t.Errorf("scenario.yaml does not carry %q:\n%s", want, scenarioYAML)
		}
	}

	stepsData, err := os.ReadFile(filepath.Join(runDir, "steps.jsonl"))
	if err != nil {
		t.Fatalf("read steps.jsonl: %v", err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(stepsData)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("steps.jsonl line is not JSON: %q (%v)", line, err)
		}
		records = append(records, rec)
	}
	if len(records) != 2 {
		t.Fatalf("steps.jsonl holds %d record(s), want one per step and assertion\n%s", len(records), stepsData)
	}
	first := records[0]
	if first["name"] != "a passing step" {
		t.Errorf("first record name = %v, want %q", first["name"], "a passing step")
	}
	for _, key := range []string{"scenario", "type", "input", "durationMs", "pass", "startedAt"} {
		if _, ok := first[key]; !ok {
			t.Errorf("step record has no %q key: %v", key, first)
		}
	}
	last := records[1]
	if last["pass"] != false {
		t.Errorf("failing assertion record pass = %v, want false", last["pass"])
	}
	if _, ok := last["failure"]; !ok {
		t.Errorf("failing assertion record has no failure detail: %v", last)
	}
}

// TestRun_FailedRun_RunJSONHoldsOutcomeTimingsAndRedactedConfig holds SDD
// section 12.1 and section 12.2 together. run.json carries the outcome, the
// timings and the resolved config, and no secret reaches it in clear text.
func TestRun_FailedRun_RunJSONHoldsOutcomeTimingsAndRedactedConfig(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	bundleDir := filepath.Join(t.TempDir(), "runs")
	path := writeScenario(t, dir, "bundle.yaml", scenarioOneStepOneFailingAssertion)

	const secret = "super-secret-value"
	cfg := env.config()
	cfg.Auth = map[string]config.AuthConfig{
		"hydra": {TokenURL: "http://127.0.0.1:9/token", ClientID: "citadel", ClientSecret: secret},
	}

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{path},
		Config:        cfg,
		BundleDir:     bundleDir,
	})
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitFailure, dumpEvents(events))
	}

	runJSON := filepath.Join(bundleDir, runIDFromEvents(t, events), "run.json")
	data, err := os.ReadFile(runJSON)
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Errorf("run.json carries the client secret in clear text:\n%s", data)
	}

	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("run.json is not JSON: %v", err)
	}
	if report["outcome"] != "fail" {
		t.Errorf("run.json outcome = %v, want %q", report["outcome"], "fail")
	}
	if report["exitCode"] != float64(ExitFailure) {
		t.Errorf("run.json exitCode = %v, want %d", report["exitCode"], ExitFailure)
	}
	for _, key := range []string{"runId", "startedAt", "endedAt", "durationMs", "config"} {
		if _, ok := report[key]; !ok {
			t.Errorf("run.json has no %q key: %v", key, report)
		}
	}
	if _, ok := report["config"].(map[string]any); !ok {
		t.Errorf("run.json config = %v, want the resolved configuration", report["config"])
	}
}

// TestRun_BundleWritten_PrunesBundlesOlderThanRetention holds the last line of
// SDD section 12.1. A bundle older than the retention period goes on write, so
// a developer machine does not fill with old runs.
func TestRun_BundleWritten_PrunesBundlesOlderThanRetention(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()
	bundleDir := filepath.Join(t.TempDir(), "runs")
	path := writeScenario(t, dir, "bundle.yaml", scenarioOneStepOneFailingAssertion)

	stale := filepath.Join(bundleDir, "0190-stale-run")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("create stale bundle: %v", err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age the stale bundle: %v", err)
	}

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{path},
		Config:        env.config(),
		BundleDir:     bundleDir,
	})
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitFailure, dumpEvents(events))
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale bundle survived the write, err = %v", err)
	}
	fresh := filepath.Join(bundleDir, runIDFromEvents(t, events))
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the new bundle is missing: %v", err)
	}
}
