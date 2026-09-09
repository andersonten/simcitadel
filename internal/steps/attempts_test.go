package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// SDD section 4.6 writes "attempts":80 into the step.fail event, and section
// 12.1 lists attempts among the fields of each steps.jsonl record. The retry
// loop held the count implicitly and threw it away, so an await that failed on
// the first attempt read the same as one that failed on the eightieth. The
// tests below hold the count to the number of probes the loop really ran.

// attemptsServer answers with a failing status until the caller reaches
// passOn, and it reports how many requests it received.
func attemptsServer(t *testing.T, passOn int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < passOn {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not ready"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "abc"}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func attemptsRunContext(srv *httptest.Server) *RunContext {
	return &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{App: &config.AppTarget{URL: srv.URL}},
		},
		HTTPClient: srv.Client(),
		Bindings:   make(map[string]any),
	}
}

// TestExecute_RetriedStep_ReportsEveryAttempt proves the count a retried step
// reports equals the number of requests its target received.
func TestExecute_RetriedStep_ReportsEveryAttempt(t *testing.T) {
	srv, calls := attemptsServer(t, 3)

	step := &scenario.Step{
		Name:   "the task is open",
		Type:   "await",
		HTTP:   &scenario.HTTPSource{Method: "GET", Path: "/test"},
		Expect: &scenario.Expect{Status: 200},
		Retry:  &scenario.RetrySpec{Timeout: "5s", Interval: "1ms"},
	}

	result, err := Execute(context.Background(), step, attemptsRunContext(srv))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the step failed although the target passed on attempt 3: %s", result.Expect.Message)
	}

	want := int(calls.Load())
	if result.Attempts != want {
		t.Errorf("the step reports %d attempt(s), and the target received %d request(s)",
			result.Attempts, want)
	}
}

// TestExecute_StepWithoutRetry_ReportsOneAttempt keeps the count honest at the
// lower bound. A step that runs once must report 1 and not 0, because 0 reads
// as "not recorded".
func TestExecute_StepWithoutRetry_ReportsOneAttempt(t *testing.T) {
	srv, calls := attemptsServer(t, 1)

	step := &scenario.Step{
		Name:   "the app answers",
		Type:   "probe",
		HTTP:   &scenario.HTTPSource{Method: "GET", Path: "/test"},
		Expect: &scenario.Expect{Status: 200},
	}

	result, err := Execute(context.Background(), step, attemptsRunContext(srv))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Attempts != 1 {
		t.Errorf("a step that ran once reports %d attempt(s), want 1", result.Attempts)
	}
	if calls.Load() != 1 {
		t.Errorf("the target received %d request(s), want 1", calls.Load())
	}
}

// TestExecute_RetryWindowExpires_ReportsEveryAttempt covers the path that SDD
// section 4.6 illustrates, where the window closes and the step fails. The
// count is the whole value of the record there, because it separates a target
// that never answered from one that answered wrongly many times.
func TestExecute_RetryWindowExpires_ReportsEveryAttempt(t *testing.T) {
	// The target never passes, so the loop runs until the window closes.
	srv, calls := attemptsServer(t, 1_000_000)

	step := &scenario.Step{
		Name:   "the task is open",
		Type:   "await",
		HTTP:   &scenario.HTTPSource{Method: "GET", Path: "/test"},
		Expect: &scenario.Expect{Status: 200},
		Retry:  &scenario.RetrySpec{Timeout: "120ms", Interval: "10ms"},
	}

	result, err := Execute(context.Background(), step, attemptsRunContext(srv))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pass {
		t.Fatal("the step passed although the target never met the expectation")
	}

	// The count is bounded rather than exact on purpose. The window can close
	// before a probe reaches the target or after it answers, so the number of
	// requests the target received is one of two values and an exact assertion
	// here is a flaky test rather than a stronger one. The bound still holds the
	// two properties that matter: the loop retried, and it counts no evaluation
	// that never happened.
	sent := int(calls.Load())
	if sent < 2 {
		t.Fatalf("the target received %d request(s), so the window closed before a retry and this test proves nothing", sent)
	}
	if result.Attempts < 2 {
		t.Errorf("the failed step reports %d attempt(s) although the window allowed several", result.Attempts)
	}
	if result.Attempts > sent {
		t.Errorf("the step reports %d attempt(s), and the target received only %d request(s)",
			result.Attempts, sent)
	}
}

// TestExecute_NonProbeStep_ReportsOneAttempt keeps every other branch of the
// dispatcher from reporting 0, which a reader cannot tell from "not recorded".
func TestExecute_NonProbeStep_ReportsOneAttempt(t *testing.T) {
	step := &scenario.Step{
		Name:     "settle",
		Type:     "wait",
		Duration: "1ms",
	}

	result, err := Execute(context.Background(), step, &RunContext{
		Config:   &config.CitadelConfig{},
		Bindings: make(map[string]any),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Attempts != 1 {
		t.Errorf("a wait step reports %d attempt(s), want 1", result.Attempts)
	}
}
