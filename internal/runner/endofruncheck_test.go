package runner

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/capture"
	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/expect"
	"github.com/andersonten/simcitadel/internal/scenario"
	"github.com/andersonten/simcitadel/internal/steps"
)

// This file holds the end-of-run environment checks of SDD section 4.11. A
// check whose target does not answer must fail the run, never report a pass.

// captureConfig returns a configuration whose only target is the capture
// service at the given address.
func captureConfig(url string) *config.CitadelConfig {
	return &config.CitadelConfig{
		Targets: config.TargetsConfig{
			EventCapture: &config.EventCaptureTarget{URL: url},
		},
	}
}

// TestCheckConformance_CaptureServiceUnreachable_FailsTheRun holds SDD section
// 4.11 and section 4.4. The conformance check is the only guard against a
// malformed event that no assertion names, and section 4.4 says "a step that
// cannot reach its target exits 3, not 1". A check that cannot reach the
// capture service proves nothing, so it must not report a pass.
func TestCheckConformance_CaptureServiceUnreachable_FailsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	err := checkConformance(context.Background(), captureConfig(closedURL), http.DefaultClient)
	if err == nil {
		t.Error("the conformance check passed although the capture service was unreachable")
	}
}

// TestCheckConformance_CaptureServiceReturns500_FailsTheRun holds the same
// requirement for a capture service that answers with an error.
func TestCheckConformance_CaptureServiceReturns500_FailsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"buffer unavailable"}`))
	}))
	t.Cleanup(srv.Close)

	err := checkConformance(context.Background(), captureConfig(srv.URL), srv.Client())
	if err == nil {
		t.Error("the conformance check passed although the capture service returned 500")
	}
}

// TestCheckConformance_NonJSONMessageBody_FailsTheRun is the sharpest case.
// SDD section 4.11 says "A malformed outbound event fails the run even if no
// assertion mentioned it". A body that is not JSON at all is the most malformed
// event the app can publish, so the check must fail the run.
func TestCheckConformance_NonJSONMessageBody_FailsTheRun(t *testing.T) {
	buf := capture.NewBuffer(100)
	buf.Add(capture.Message{
		ReceivedAt: time.Now(),
		Exchange:   "public.orchestration-engine.Manuscript.v1",
		RoutingKey: "public.orchestration-engine.Manuscript.v1",
		Headers:    map[string]string{},
		RunID:      "run-endofrun",
		Body:       json.RawMessage("<xml>not an event</xml>"),
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(capture.NewServer(buf, func() map[string]string { return nil }, logger).Handler())
	t.Cleanup(srv.Close)

	// Show what the capture service actually returns, so the cause is visible.
	resp, err := srv.Client().Get(srv.URL + "/captured")
	if err != nil {
		t.Fatalf("query the capture service: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("GET /captured returned status %d and body %q", resp.StatusCode, string(body))

	err = checkConformance(context.Background(), captureConfig(srv.URL), srv.Client())
	if err == nil {
		t.Error("the conformance check passed although one captured message had no JSON body")
	}
}

// TestCheckUnroutableDepth_Failure_NamesTheExchangeAndRoutingKey holds SDD
// section 4.11: "If citadel.unroutable (§7.4) is non-empty, fail and list the
// exchange and routing key of each message." The check reads only the queue
// depth, so the failure names neither.
func TestCheckUnroutableDepth_Failure_NamesTheExchangeAndRoutingKey(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"messages":2}`))
	}))
	t.Cleanup(srv.Close)

	err := checkUnroutableDepth(context.Background(), mgmtTarget(srv.URL), srv.Client())
	if err == nil {
		t.Fatal("a non-empty unroutable queue did not fail the run")
	}
	t.Logf("management endpoints the check called: %v", paths)
	t.Logf("failure text: %s", err.Error())

	for _, want := range []string{"exchange", "routing key"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("the failure text does not list the %s of each message: %q", want, err.Error())
		}
	}
}

// TestCheckUnroutableDepth_ManagementAPIError_FailsTheRun holds the same
// degrade rule as the conformance check. A management API that answers 500
// gives no evidence that the queue is empty.
func TestCheckUnroutableDepth_ManagementAPIError_FailsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	err := checkUnroutableDepth(context.Background(), mgmtTarget(srv.URL), srv.Client())
	if err == nil {
		t.Error("the unroutable check passed although the management API returned 500")
	}
}

// TestWriteJUnitFromRecords_EachScenario_IsOneTestSuite holds SDD section 4.6:
// "a pipeline that understands JUnit renders a scenario as a test suite". Two
// scenarios in one run must produce two suites, each named after its scenario,
// and each suite must carry the summed time of its cases.
func TestWriteJUnitFromRecords_EachScenario_IsOneTestSuite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junit.xml")
	writeJUnitFromRecords([]StepRecord{
		{Name: "put the stubs in the pre-submission state", ClassName: "editorial-assignment-happy-path", DurationSecs: 0.5, Passed: true},
		{Name: "the task is open", ClassName: "editorial-assignment-happy-path", DurationSecs: 1.5, Passed: true},
		{Name: "the reviewer declines", ClassName: "reviewer-decline", DurationSecs: 2.0, Passed: true},
	}, path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the junit report: %v", err)
	}
	t.Logf("junit report:\n%s", string(data))

	var suites xmlTestSuites
	if err := xml.Unmarshal(data, &suites); err != nil {
		t.Fatalf("parse the junit report: %v", err)
	}

	if len(suites.Suites) != 2 {
		t.Errorf("the report holds %d test suite(s) for 2 scenarios", len(suites.Suites))
	}
	for _, s := range suites.Suites {
		if s.Name == "citadel" {
			t.Errorf("a suite is named %q rather than after its scenario", s.Name)
		}
		if s.Time == 0 {
			t.Errorf("suite %q reports time 0 although its cases total more than zero", s.Name)
		}
	}
}

// TestStepFailEvent_Failure_CarriesAttemptsAndJSONPath holds the schema of SDD
// section 4.6. The documented step.fail event is
//
//	"failure":{"class":"assertion","expected":"OPEN","actual":"null",
//	"jsonPath":"$.data.pe_tasks...","attempts":80}
//
// buildFailureMap emits message, expected and actual only, so a pipeline that
// reads the documented keys finds neither the retry count nor the path.
func TestStepFailEvent_Failure_CarriesAttemptsAndJSONPath(t *testing.T) {
	var out strings.Builder
	emitter := NewEmitter(&out, "json", "run-endofrun")

	record := StepRecord{
		Name:         "the task is open",
		Type:         "await",
		Passed:       false,
		FailureClass: "assertion",
		Failure: map[string]any{
			"message":  "jsonPath $.data.pe_tasks[0].status: expected OPEN, got null",
			"expected": "OPEN",
			"actual":   nil,
		},
	}
	emitter.StepFail(4, 20001, record.Failure, record.FailureClass)

	var evt struct {
		Failure map[string]any `json:"failure"`
	}
	if err := json.Unmarshal([]byte(out.String()), &evt); err != nil {
		t.Fatalf("parse the step.fail event: %v", err)
	}
	t.Logf("step.fail event: %s", strings.TrimSpace(out.String()))

	for _, key := range []string{"jsonPath", "attempts"} {
		if _, ok := evt.Failure[key]; !ok {
			t.Errorf("the failure object has no %q key", key)
		}
	}
}

// TestRecordStep_RetriedStep_RecordsTheAttemptCount holds the same schema and
// the steps.jsonl record of SDD section 12.1, which holds "inputs, outputs,
// attempts, timings". recordStep once set Attempts to 1 only when the step had
// no retry policy, so exactly the step that retries recorded no count. The
// retry loop of internal/steps now counts its evaluations, so the record holds
// the number rather than a null that means "not recorded".
func TestRecordStep_RetriedStep_RecordsTheAttemptCount(t *testing.T) {
	record := StepRecord{
		Name:     "the task is open",
		Type:     "await",
		Retry:    map[string]any{"timeout": "20s", "interval": "250ms"},
		Attempts: 80,
	}
	rendered := record.bundleRecord()
	t.Logf("steps.jsonl record: %v", rendered)

	attempts, ok := rendered["attempts"]
	if !ok {
		t.Fatal("a retried step records no attempts key, so the retry count is lost")
	}
	if attempts != 80 {
		t.Errorf("the record reports attempts %v, want 80", attempts)
	}
}

// TestRecordStep_RetriedStep_CarriesTheCountFromTheStepResult holds the wiring
// between the two packages. The count travels from the retry loop through
// steps.StepResult, and a record that reads the retry policy in place of the
// result reports a policy rather than a fact.
func TestRecordStep_RetriedStep_CarriesTheCountFromTheStepResult(t *testing.T) {
	step := &scenario.Step{
		Name:   "the task is open",
		Type:   "await",
		Retry:  &scenario.RetrySpec{Timeout: "20s", Interval: "250ms"},
		Expect: &scenario.Expect{JSONPathExists: "$.data.pe_tasks[0]"},
	}
	result := &steps.StepResult{
		Pass:     false,
		Attempts: 80,
		Expect:   &expect.Result{Pass: false, Message: "jsonPathExists: no match"},
	}

	record := recordStep(&scenario.Scenario{Name: "editorial-assignment-happy-path"},
		step, 4, time.Now(), result, nil)

	if record.Attempts != 80 {
		t.Errorf("the record reports %d attempt(s), and the step result carried 80", record.Attempts)
	}
	if got := record.bundleRecord()["attempts"]; got != 80 {
		t.Errorf("the steps.jsonl record reports attempts %v, want 80", got)
	}
	if got := record.Failure["attempts"]; got != 80 {
		t.Errorf("the failure object reports attempts %v, want 80", got)
	}
}

// unroutableManagement serves the two management endpoints that the unroutable
// check of SDD section 4.11 reads. It answers as RabbitMQ does, and records the
// body of the message read, so a test can hold the read to ack_requeue_true.
type unroutableManagement struct {
	server *httptest.Server

	mu   sync.Mutex
	body map[string]any
}

func newUnroutableManagement(t *testing.T, depth int, messages string) *unroutableManagement {
	t.Helper()
	mgmt := &unroutableManagement{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/queues", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/queues/%2F/citadel.lock", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"citadel.lock","messages":0}`))
	})
	mux.HandleFunc("/api/queues/%2F/citadel.unroutable", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"name":"citadel.unroutable","messages":%d}`, depth)
	})
	mux.HandleFunc("/api/queues/%2F/citadel.unroutable/get", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mgmt.mu.Lock()
		mgmt.body = body
		mgmt.mu.Unlock()
		_, _ = w.Write([]byte(messages))
	})

	mgmt.server = httptest.NewServer(mux)
	t.Cleanup(mgmt.server.Close)
	return mgmt
}

// readBody returns the body of the message read the check sent.
func (m *unroutableManagement) readBody() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.body
}

// unroutableSample is the answer of POST /api/queues/%2F/citadel.unroutable/get
// for two messages, in the shape the RabbitMQ management API returns.
const unroutableSample = `[
  {"payload_bytes":42,"redelivered":false,"exchange":"public.orchestration-engine.Article.v1",
   "routing_key":"article.submitted","message_count":1,"properties":{"headers":{}},
   "payload":"{}","payload_encoding":"string"},
  {"payload_bytes":42,"redelivered":false,"exchange":"private.orchestration-engine.Task.v1",
   "routing_key":"task.opened","message_count":0,"properties":{"headers":{}},
   "payload":"{}","payload_encoding":"string"}
]`

// TestCheckUnroutableDepth_NonEmptyQueue_NamesEachExchangeAndRoutingKey holds
// SDD section 4.11 against a management API that answers as RabbitMQ does. The
// failure must name the exchange and the routing key of every message, because
// those two facts are what a developer needs to fix the topology.
func TestCheckUnroutableDepth_NonEmptyQueue_NamesEachExchangeAndRoutingKey(t *testing.T) {
	mgmt := newUnroutableManagement(t, 2, unroutableSample)

	err := checkUnroutableDepth(context.Background(), mgmtTarget(mgmt.server.URL), mgmt.server.Client())
	if err == nil {
		t.Fatal("a non-empty unroutable queue did not fail the run")
	}
	t.Logf("failure text: %s", err.Error())

	for _, want := range []string{
		"public.orchestration-engine.Article.v1",
		"article.submitted",
		"private.orchestration-engine.Task.v1",
		"task.opened",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure text does not name %q: %q", want, err.Error())
		}
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d, because a broken topology is an environment problem", got, ExitEnvironment)
	}

	// The read must put every message back, so the diagnostic bundle and a
	// second look at the queue still hold them.
	if got := mgmt.readBody()["ackmode"]; got != "ack_requeue_true" {
		t.Errorf("ackmode = %v, want \"ack_requeue_true\", so the read is non-destructive", got)
	}
}

// TestCheckUnroutableDepth_QueueAbsent_FailsTheRun holds SDD section 7.3, which
// declares the queue. A broker without it has nowhere to put an unroutable
// message, so the check proves nothing and must not report a pass.
func TestCheckUnroutableDepth_QueueAbsent_FailsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	err := checkUnroutableDepth(context.Background(), mgmtTarget(srv.URL), srv.Client())
	if err == nil {
		t.Fatal("the unroutable check passed although the queue is not declared")
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d", got, ExitEnvironment)
	}
}

// TestCheckUnconsumedDepth_ManagementAPIError_FailsTheRun holds the degrade
// rule of finding 1 for the unconsumed check. A queue list that never arrives
// is no evidence that every queue is empty.
func TestCheckUnconsumedDepth_ManagementAPIError_FailsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{RabbitMQ: &config.RabbitMQTarget{ManagementURL: srv.URL}},
	}
	err := checkUnconsumedDepth(context.Background(), cfg, srv.Client())
	if err == nil {
		t.Fatal("the unconsumed check passed although the management API returned 503")
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d", got, ExitEnvironment)
	}
}

// TestCheckUnmatchedRequests_WireMockUnreachable_FailsTheRun holds the same
// rule for the WireMock check.
func TestCheckUnmatchedRequests_WireMockUnreachable_FailsTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{URL: closedURL, AdminURL: closedURL + "/__admin"},
		},
	}
	err := checkUnmatchedRequests(context.Background(), cfg, http.DefaultClient)
	if err == nil {
		t.Fatal("the unmatched requests check passed although WireMock was unreachable")
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d", got, ExitEnvironment)
	}
}

// TestReportEndOfRunFailures_EnvironmentFailure_ExitsThreeAndSaysWhy holds SDD
// section 4.4 and section 4.11 together. Exit 1 is the only code that indicates
// a finding about the app, so a broken environment exits 3 and a malformed
// envelope exits 1. Both reasons reach the JSON stream, because a consumer that
// reads run.end alone cannot tell what failed.
func TestReportEndOfRunFailures_EnvironmentFailure_ExitsThreeAndSaysWhy(t *testing.T) {
	cases := []struct {
		name     string
		failures []checkFailure
		want     int
		wantText string
	}{
		{
			name:     "an unroutable message is an environment problem",
			failures: []checkFailure{{Check: checkNameUnroutable, Err: &EnvironmentError{Msg: "queue citadel.unroutable holds 2 message(s)"}}},
			want:     ExitEnvironment,
			wantText: "citadel.unroutable",
		},
		{
			name:     "a malformed envelope is a finding about the app",
			failures: []checkFailure{{Check: checkNameConformance, Err: &StepFailureError{Msg: "conformance: captured message 0 has no JSON body"}}},
			want:     ExitFailure,
			wantText: "conformance",
		},
		{
			name: "a finding about the app wins over an environment problem",
			failures: []checkFailure{
				{Check: checkNameUnmatched, Err: &EnvironmentError{Msg: "WireMock recorded 2 unmatched request(s)"}},
				{Check: checkNameConformance, Err: &StepFailureError{Msg: "conformance: missing attribute \"version\""}},
			},
			want:     ExitFailure,
			wantText: "version",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			got := reportEndOfRunFailures(NewEmitter(&out, "json", "run-endofrun"), tc.failures)
			t.Logf("event stream:\n%s", out.String())

			if got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
			if count := strings.Count(out.String(), `"type":"check.fail"`); count != len(tc.failures) {
				t.Errorf("check.fail count = %d, want %d", count, len(tc.failures))
			}
			if !strings.Contains(out.String(), tc.wantText) {
				t.Errorf("the event stream does not say why the run failed: %s", out.String())
			}
		})
	}
}

// TestRun_UnroutableMessage_ExitsEnvironmentAndNamesTheCheck is the end-to-end
// form of finding 6. A run whose steps all pass, and whose broker holds an
// unroutable message, must exit 3 and must tell a consumer of the JSON stream
// which check failed.
func TestRun_UnroutableMessage_ExitsEnvironmentAndNamesTheCheck(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	mgmt := newUnroutableManagement(t, 2, unroutableSample)
	dir := t.TempDir()
	path := writeScenario(t, dir, "a.yaml", scenarioAllPass)

	cfg := env.config()
	cfg.Targets.RabbitMQ.ManagementURL = mgmt.server.URL

	code, events := runCapture(t, RunConfig{ScenarioPaths: []string{path}, Config: cfg})

	if code != ExitEnvironment {
		t.Errorf("exit = %d, want %d\n%s", code, ExitEnvironment, dumpEvents(events))
	}
	checks := eventsOfType(events, "check.fail")
	if len(checks) != 1 {
		t.Fatalf("check.fail count = %d, want 1\n%s", len(checks), dumpEvents(events))
	}
	if checks[0]["check"] != checkNameUnroutable {
		t.Errorf("check = %v, want %q", checks[0]["check"], checkNameUnroutable)
	}
	failure, _ := checks[0]["failure"].(map[string]any)
	if failure["class"] != "environment" {
		t.Errorf("failure.class = %v, want \"environment\"", failure["class"])
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "article.submitted") {
		t.Errorf("failure.message = %v, want the routing key of the unroutable message", failure["message"])
	}
}

// TestRecordStep_FailedAssertion_CarriesTheJSONPathAndTheAttemptCount holds the
// step.fail schema of SDD section 4.6 for the record the runner builds. The
// path comes from the expectation the step declares, and the count comes from
// the step result, where steps.Execute reports 1 for a step that ran once.
func TestRecordStep_FailedAssertion_CarriesTheJSONPathAndTheAttemptCount(t *testing.T) {
	step := &scenario.Step{
		Name: "the task is open",
		Type: "app/graphql",
		Expect: &scenario.Expect{
			JSONPath: "$.data.pe_tasks[0].status",
			Equals:   "OPEN",
		},
	}
	result := &steps.StepResult{
		Pass:     false,
		Attempts: 1,
		Expect: &expect.Result{
			Pass:     false,
			Expected: "OPEN",
			Message:  "jsonpath $.data.pe_tasks[0].status: expected OPEN, got <nil>",
		},
	}

	record := recordStep(&scenario.Scenario{Name: "editorial-assignment-happy-path"}, step, 4, time.Now(), result, nil)
	t.Logf("failure object: %v", record.Failure)

	if record.Failure["jsonPath"] != "$.data.pe_tasks[0].status" {
		t.Errorf("failure.jsonPath = %v, want the path of the expectation", record.Failure["jsonPath"])
	}
	if record.Failure["attempts"] != 1 {
		t.Errorf("failure.attempts = %v, want 1 for a step with no retry policy", record.Failure["attempts"])
	}
}
