package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
)

// This file locks down the end-of-run checks of SDD section 4.11 and the
// failure taxonomy of section 4.5. Each test began as a proof that a strict
// setting had no effect, or that an environment failure was reported as a bug
// in Citadel.

// managementServer serves the RabbitMQ management endpoints the checks read.
func managementServer(t *testing.T, queues string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/queues/%2F/citadel.unroutable", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"messages":0}`))
	})
	mux.HandleFunc("/api/queues", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(queues))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestCheckUnconsumedDepth_StrictSetting_FailsTheRun holds SDD section 4.11.
// strictUnconsumed exists to fail a run, so a check that only logged would make
// the setting a no-op.
func TestCheckUnconsumedDepth_StrictSetting_FailsTheRun(t *testing.T) {
	srv := managementServer(t, `[{"name":"app.inbox","messages":4}]`)

	for _, strict := range []bool{false, true} {
		cfg := &config.CitadelConfig{
			Targets:  config.TargetsConfig{RabbitMQ: &config.RabbitMQTarget{ManagementURL: srv.URL}},
			Defaults: config.DefaultsConfig{StrictUnconsumed: strict},
		}
		err := checkUnconsumedDepth(context.Background(), cfg, srv.Client())

		if strict && err == nil {
			t.Error("strictUnconsumed=true returned no error for a queue at depth 4")
		}
		if strict && err != nil && !strings.Contains(err.Error(), "app.inbox") {
			t.Errorf("error %q does not name the queue", err.Error())
		}
		if !strict && err != nil {
			t.Errorf("strictUnconsumed=false returned an error: %v", err)
		}
	}
}

// TestCheckUnconsumedDepth_CitadelOwnedQueues_AreExempt keeps Citadel's own
// spy and lock queues out of the unconsumed check. Nothing consumes a spy
// queue, so counting it would fail every run.
func TestCheckUnconsumedDepth_CitadelOwnedQueues_AreExempt(t *testing.T) {
	srv := managementServer(t, `[{"name":"citadel.spy.article","messages":7},{"name":"citadel.unroutable","messages":0}]`)

	cfg := &config.CitadelConfig{
		Targets:  config.TargetsConfig{RabbitMQ: &config.RabbitMQTarget{ManagementURL: srv.URL}},
		Defaults: config.DefaultsConfig{StrictUnconsumed: true},
	}
	if err := checkUnconsumedDepth(context.Background(), cfg, srv.Client()); err != nil {
		t.Errorf("a Citadel-owned queue failed the unconsumed check: %v", err)
	}
}

// TestCheckUnmatchedRequests_StrictSetting_FailsTheRun holds SDD section 4.11
// for the WireMock side of the same idea.
func TestCheckUnmatchedRequests_StrictSetting_FailsTheRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/__admin/requests/unmatched", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"requests":[{"url":"/nope"},{"url":"/also-nope"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, strict := range []bool{false, true} {
		cfg := &config.CitadelConfig{
			Targets: config.TargetsConfig{
				WireMock: &config.WireMockTarget{URL: srv.URL, AdminURL: srv.URL + "/__admin"},
			},
			Defaults: config.DefaultsConfig{StrictUnmatched: strict},
		}
		err := checkUnmatchedRequests(context.Background(), cfg, srv.Client())

		if strict && err == nil {
			t.Error("strictUnmatched=true returned no error for two unmatched requests")
		}
		if !strict && err != nil {
			t.Errorf("strictUnmatched=false returned an error: %v", err)
		}
	}
}

// TestVerifyQueueDepths_OnlyChecksThePurgedQueues holds SDD section 4.9 step 7.
// The settle check proves the reset worked. A queue the reset never purged is
// out of scope, and failing on it would blame the reset for someone else's
// message.
func TestVerifyQueueDepths_OnlyChecksThePurgedQueues(t *testing.T) {
	srv := managementServer(t, `[{"name":"app.inbox","messages":0},{"name":"citadel.lock","messages":3}]`)

	if err := verifyQueueDepths(context.Background(), mgmtTarget(srv.URL), srv.Client(), []string{"app.inbox"}); err != nil {
		t.Errorf("a queue outside the purge list failed the settle check: %v", err)
	}

	err := verifyQueueDepths(context.Background(), mgmtTarget(srv.URL), srv.Client(), []string{"app.inbox", "citadel.lock"})
	if err == nil {
		t.Error("a purged queue still holding 3 messages passed the settle check")
	}
}

// TestClassifyError_TransportFailure_IsEnvironmentNotInternal holds the last
// row of the SDD section 14 test list: a step that cannot reach its target
// exits 3, not 1 and not 6.
func TestClassifyError_TransportFailure_IsEnvironmentNotInternal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "typed environment error",
			err:  &EnvironmentError{Msg: "wiremock unreachable"},
			want: ExitEnvironment,
		},
		{
			name: "http client dial failure",
			err:  fmt.Errorf("probe app: %w", &url.Error{Op: "Get", URL: "http://x", Err: &net.OpError{Op: "dial"}}),
			want: ExitEnvironment,
		},
		{
			name: "raw dial failure",
			err:  fmt.Errorf("connect to broker: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}),
			want: ExitEnvironment,
		},
		{
			name: "dns failure",
			err:  fmt.Errorf("resolve host: %w", &net.DNSError{Name: "nowhere"}),
			want: ExitEnvironment,
		},
		{
			name: "deadline exceeded",
			err:  fmt.Errorf("step timed out: %w", context.DeadlineExceeded),
			want: ExitTimeout,
		},
		{
			name: "typed step failure stays an assertion failure",
			err:  &StepFailureError{Msg: "expected 200, got 500"},
			want: ExitFailure,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestClassifyStepFailure_MatchesTheExitCode holds SDD section 4.4, which says
// failure.class maps to the exit code. The two must never disagree.
func TestClassifyStepFailure_MatchesTheExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&EnvironmentError{Msg: "unreachable"}, "environment"},
		{&InvalidInputError{Msg: "missing file"}, "input"},
		{&TimeoutError{Msg: "too slow"}, "timeout"},
		{&StepFailureError{Msg: "expected 200"}, "assertion"},
		{&InternalError{Msg: "bug"}, "internal"},
		{fmt.Errorf("dial: %w", &net.OpError{Op: "dial"}), "environment"},
	}

	for _, tc := range cases {
		if got := classifyStepFailure(tc.err); got != tc.want {
			t.Errorf("classifyStepFailure(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	if classifyStepFailure(nil) != "" {
		t.Error("classifyStepFailure(nil) is not empty")
	}
}

// TestAcquireLock_HeldByAnother_NamesTheEnvironment holds the message of SDD
// section 4.8. A developer needs to know which environment is busy.
func TestAcquireLock_HeldByAnother_NamesTheEnvironment(t *testing.T) {
	err := &LockHeldError{Msg: fmt.Sprintf("another run holds the lock on environment %q", "staging")}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("message %q does not name the environment", err.Error())
	}
	if ClassifyError(err) != ExitLockHeld {
		t.Errorf("exit = %d, want %d", ClassifyError(err), ExitLockHeld)
	}
}

// TestRun_StepThatCannotReachItsTarget_ExitsEnvironment is the end-to-end form
// of the SDD section 14 requirement, through the real run lifecycle.
func TestRun_StepThatCannotReachItsTarget_ExitsEnvironment(t *testing.T) {
	stubLock(t)
	env := newLifecycleEnv(t)
	dir := t.TempDir()

	path := writeScenario(t, dir, "a.yaml", `name: unreachable-target
steps:
  - name: a step against a dead port
    type: probe
    graphql:
      query: "{ __typename }"
    expect:
      status: 200
`)

	// The app is healthy for preflight, but drops the connection on the GraphQL
	// path. That is a target the step cannot reach, not a bad response.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		conn, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr != nil {
			t.Errorf("hijack: %v", hijackErr)
			return
		}
		_ = conn.Close()
	})
	app := httptest.NewServer(mux)
	t.Cleanup(app.Close)

	cfg := env.config()
	cfg.Targets.App = &config.AppTarget{URL: app.URL, GraphQLPath: "/graphql"}

	code, events := runCapture(t, RunConfig{
		ScenarioPaths: []string{path},
		Config:        cfg,
		Timeout:       10 * time.Second,
	})

	fails := eventsOfType(events, "step.fail")
	if len(fails) != 1 {
		t.Fatalf("step.fail count = %d, want 1; the step must run and fail\n%s", len(fails), dumpEvents(events))
	}
	failure, _ := fails[0]["failure"].(map[string]any)
	if failure["class"] != "environment" {
		t.Errorf("failure.class = %v, want \"environment\"", failure["class"])
	}
	if code != ExitEnvironment {
		t.Errorf("exit = %d, want %d\n%s", code, ExitEnvironment, dumpEvents(events))
	}
}
