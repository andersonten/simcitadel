package steps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the negative assertion of SDD section 8.3. These tests
// began as a proof that routingKeyPattern and jsonPath never reached the
// capture service, so an assertion whose only selector was one of the two
// matched every message and passed while it checked something the author never
// wrote.

// captureQueryServer answers every capture query with the given body and
// records the query string of each request.
func captureQueryServer(t *testing.T, body string) (*httptest.Server, func() []url.Values) {
	t.Helper()
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []url.Values { return queries }
}

// notCapturedContext returns a run context that reaches the capture service at
// the given address and keeps the quiet period short.
func notCapturedContext(t *testing.T, captureURL string, client *http.Client) *RunContext {
	t.Helper()
	return &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				EventCapture: &config.EventCaptureTarget{URL: captureURL},
			},
		},
		RunID:      "run-1",
		RunStart:   time.Now(),
		HTTPClient: client,
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}
}

// TestExecuteNotCaptured_PatternAndJSONPathSelectors_ReachTheCaptureService
// holds the answer of SDD section 8.3 to the classic defect of a negative assertion: a selector that is
// present is applied by the capture service, where it cannot be dropped.
func TestExecuteNotCaptured_PatternAndJSONPathSelectors_ReachTheCaptureService(t *testing.T) {
	srv, queries := captureQueryServer(t, "[]")

	rc := notCapturedContext(t, srv.URL, srv.Client())
	step := &scenario.Step{
		Name:        "no rejection event reached the broker",
		Type:        "event/not-captured",
		QuietPeriod: "1ms",
		Captured: &scenario.CapturedQuery{
			RoutingKeyPattern: "public.*.Article.v1",
			JSONPath:          "$.data.articleId",
		},
	}

	result, err := executeNotCaptured(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("executeNotCaptured: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the assertion failed against an empty capture service: %v", result.Err)
	}

	sent := queries()
	if len(sent) != 2 {
		t.Fatalf("the assertion made %d queries, want the two of SDD section 8.3", len(sent))
	}
	for i, q := range sent {
		if got := q.Get("routingKeyPattern"); got != "public.*.Article.v1" {
			t.Errorf("query %d routingKeyPattern = %q, want the selector the step wrote", i, got)
		}
		if got := q.Get("jsonPath"); got != "$.data.articleId" {
			t.Errorf("query %d jsonPath = %q, want the selector the step wrote", i, got)
		}
	}
}

// TestExecuteNotCaptured_EverySelectorShape_ReachesTheCaptureService audits the
// whole selector set of SDD section 8.3 against the query the assertion sends.
// A selector a scenario may write must arrive under its own parameter name.
func TestExecuteNotCaptured_EverySelectorShape_ReachesTheCaptureService(t *testing.T) {
	cases := []struct {
		name  string
		step  *scenario.Step
		param string
		want  string
	}{
		{
			name:  "exchange on the step",
			step:  &scenario.Step{Exchange: "public.a.v1"},
			param: "exchange",
			want:  "public.a.v1",
		},
		{
			name:  "exchange under captured",
			step:  &scenario.Step{Captured: &scenario.CapturedQuery{Exchange: "public.a.v1"}},
			param: "exchange",
			want:  "public.a.v1",
		},
		{
			name:  "routingKey on the step",
			step:  &scenario.Step{RoutingKey: "article-rejected"},
			param: "routingKey",
			want:  "article-rejected",
		},
		{
			name:  "routingKey under captured",
			step:  &scenario.Step{Captured: &scenario.CapturedQuery{RoutingKey: "article-rejected"}},
			param: "routingKey",
			want:  "article-rejected",
		},
		{
			name:  "routingKeyPattern under captured",
			step:  &scenario.Step{Captured: &scenario.CapturedQuery{RoutingKeyPattern: "public.#"}},
			param: "routingKeyPattern",
			want:  "public.#",
		},
		{
			name:  "eventType on the step",
			step:  &scenario.Step{EventType: "ArticleRejected"},
			param: "eventType",
			want:  "ArticleRejected",
		},
		{
			name:  "eventType under captured",
			step:  &scenario.Step{Captured: &scenario.CapturedQuery{EventType: "ArticleRejected"}},
			param: "eventType",
			want:  "ArticleRejected",
		},
		{
			name:  "eventTypePattern on the step",
			step:  &scenario.Step{EventTypePattern: "*Rejected*"},
			param: "eventTypePattern",
			want:  "*Rejected*",
		},
		{
			name:  "eventTypePattern under captured",
			step:  &scenario.Step{Captured: &scenario.CapturedQuery{EventTypePattern: "*Rejected*"}},
			param: "eventTypePattern",
			want:  "*Rejected*",
		},
		{
			name:  "jsonPath under captured",
			step:  &scenario.Step{Captured: &scenario.CapturedQuery{JSONPath: "$.data.articleId"}},
			param: "jsonPath",
			want:  "$.data.articleId",
		},
		{
			name:  "header filter on the step",
			step:  &scenario.Step{HeaderFilter: map[string]string{"aggregateId": "515098"}},
			param: "header.aggregateId",
			want:  "515098",
		},
		{
			name:  "header filter under captured",
			step:  &scenario.Step{Captured: &scenario.CapturedQuery{Headers: map[string]string{"aggregateId": "515098"}}},
			param: "header.aggregateId",
			want:  "515098",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := notCapturedContext(t, "http://127.0.0.1:1", http.DefaultClient)
			params, _ := buildNotCapturedParams(tc.step, rc)
			if got := params.Get(tc.param); got != tc.want {
				t.Errorf("query parameter %s = %q, want %q", tc.param, got, tc.want)
			}
		})
	}
}

// TestExecute_CapturedStepWithStepLevelSelectors_ForwardsThemAll audits the
// positive assertion for the same class of defect. An event/captured step
// writes its selectors on the step or under captured:, and every one of them
// must reach the capture query engine.
func TestExecute_CapturedStepWithStepLevelSelectors_ForwardsThemAll(t *testing.T) {
	srv, queries := captureQueryServer(t, "[]")

	rc := notCapturedContext(t, srv.URL, srv.Client())
	zero := 0
	step := &scenario.Step{
		Name:             "the engine published no rejection",
		Type:             "event/captured",
		Exchange:         "public.orchestration-engine.Article.v1",
		EventTypePattern: "*Rejected*",
		HeaderFilter:     map[string]string{"aggregateId": "515150"},
		Expect:           &scenario.Expect{Count: &zero},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the assertion failed against an empty capture service: %v", result.Expect)
	}

	sent := queries()
	if len(sent) != 1 {
		t.Fatalf("the step made %d queries, want 1", len(sent))
	}
	if got := sent[0].Get("eventTypePattern"); got != "*Rejected*" {
		t.Errorf("eventTypePattern = %q, want the selector the step wrote", got)
	}
	if got := sent[0].Get("header.aggregateId"); got != "515150" {
		t.Errorf("header.aggregateId = %q, want the selector the step wrote", got)
	}
}

// TestExecuteNotCaptured_Failure_NamesTheAppliedSelector holds the last
// sentence of SDD section 8.3: the assertion reports which selector it applied.
func TestExecuteNotCaptured_Failure_NamesTheAppliedSelector(t *testing.T) {
	srv, _ := captureQueryServer(t, `[{"seq":1,"exchange":"public.a.v1"}]`)

	rc := notCapturedContext(t, srv.URL, srv.Client())
	step := &scenario.Step{
		Name:        "no rejection event reached the broker",
		Type:        "event/not-captured",
		QuietPeriod: "1ms",
		Captured:    &scenario.CapturedQuery{RoutingKeyPattern: "public.*.Article.v1"},
	}

	result, err := executeNotCaptured(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("executeNotCaptured: %v", err)
	}
	if result.Pass {
		t.Fatal("the assertion passed while the capture service held a match")
	}
	if result.Err == nil {
		t.Fatal("the failed assertion reports no error")
	}
	if !strings.Contains(result.Err.Error(), "routingKeyPattern=public.*.Article.v1") {
		t.Errorf("failure message %q does not name the applied selector", result.Err.Error())
	}
}

// TestExecuteNotCaptured_Pass_ReportsTheAppliedSelector keeps the pass path
// answerable too, so the run output shows what was checked.
func TestExecuteNotCaptured_Pass_ReportsTheAppliedSelector(t *testing.T) {
	srv, _ := captureQueryServer(t, "[]")

	rc := notCapturedContext(t, srv.URL, srv.Client())
	step := &scenario.Step{
		Name:        "no rejection event reached the broker",
		Type:        "event/not-captured",
		QuietPeriod: "1ms",
		EventType:   "ArticleRejected",
	}

	result, err := executeNotCaptured(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("executeNotCaptured: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the assertion failed against an empty capture service: %v", result.Err)
	}
	if result.Expect == nil {
		t.Fatal("the passed assertion reports nothing about what it checked")
	}
	if !strings.Contains(result.Expect.Message, "eventType=ArticleRejected") {
		t.Errorf("pass message %q does not name the applied selector", result.Expect.Message)
	}
}
