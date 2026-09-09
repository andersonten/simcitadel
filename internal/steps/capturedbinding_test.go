package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the receiving half of SDD section 6.4: for an
// event/captured step the binding is the matched message. These tests began as
// a proof that the step bound whatever the last expectation key produced, an
// int for a count, so .accepted.id could never resolve after a receiving step.

// capturedEntry returns one capture service entry in the shape of SDD section 8.2.
func capturedEntry() map[string]any {
	return map[string]any{
		"seq":        412,
		"receivedAt": "2026-08-19T09:00:12.500Z",
		"exchange":   "public.orchestration-engine.Article.v1",
		"routingKey": "public.orchestration-engine.Article.v1",
		"headers":    map[string]any{"eventType": "EditorAssigned", "aggregateId": "515150"},
		"runId":      "run-1",
		"body": map[string]any{
			"specversion": "1.0",
			"type":        "EditorAssigned",
			"source":      "ORCHESTRATION-ENGINE",
			"id":          "01ABCDEF",
			"version":     "v1",
			"data":        map[string]any{"articleId": 515150},
		},
	}
}

// capturedRunContext returns a run context whose capture service answers with
// the given entries.
func capturedRunContext(t *testing.T, entries []any) *RunContext {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	t.Cleanup(srv.Close)

	return &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			EventCapture: &config.EventCaptureTarget{URL: srv.URL},
		}},
		RunID:      "run-1",
		HTTPClient: srv.Client(),
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}
}

// capturedStep returns an event/captured step that binds its match.
func capturedStep() *scenario.Step {
	atLeastOne := 1
	return &scenario.Step{
		Name:     "the engine assigned an editor",
		Type:     "event/captured",
		As:       "accepted",
		Exchange: "public.orchestration-engine.Article.v1",
		Expect:   &scenario.Expect{CountAtLeast: &atLeastOne},
	}
}

// TestExecute_CapturedStep_BindsTheMatchedMessage holds the documented fields
// of SDD section 6.4 for a receiving step.
func TestExecute_CapturedStep_BindsTheMatchedMessage(t *testing.T) {
	rc := capturedRunContext(t, []any{capturedEntry()})

	result, err := Execute(context.Background(), capturedStep(), rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the captured step failed: %v", result.Expect)
	}

	binding, ok := rc.Bindings["accepted"].(map[string]any)
	if !ok {
		t.Fatalf("the binding is %T, want the map a template reads", rc.Bindings["accepted"])
	}
	if binding["id"] != "01ABCDEF" {
		t.Errorf("id = %v, want the CloudEvents id of the matched message", binding["id"])
	}
	if binding["exchange"] != "public.orchestration-engine.Article.v1" {
		t.Errorf("exchange = %v, want the exchange of the matched message", binding["exchange"])
	}
	if binding["routingKey"] != "public.orchestration-engine.Article.v1" {
		t.Errorf("routingKey = %v, want the routing key of the matched message", binding["routingKey"])
	}
	if binding["receivedAt"] != "2026-08-19T09:00:12.500Z" {
		t.Errorf("receivedAt = %v, want the receive time of the matched message", binding["receivedAt"])
	}
	if _, unexpected := binding["sentAt"]; unexpected {
		t.Error("a received message reports sentAt")
	}
	headers, ok := binding["headers"].(map[string]string)
	if !ok {
		t.Fatalf("headers is %T, want a string map", binding["headers"])
	}
	if headers["eventType"] != "EditorAssigned" {
		t.Errorf("headers.eventType = %q, want EditorAssigned", headers["eventType"])
	}
}

// TestExecute_StepAfterACapturedStep_ResolvesTheBinding is the point of the
// binding: a later step addresses the message the assertion matched.
func TestExecute_StepAfterACapturedStep_ResolvesTheBinding(t *testing.T) {
	rc := capturedRunContext(t, []any{capturedEntry()})

	if _, err := Execute(context.Background(), capturedStep(), rc); err != nil {
		t.Fatalf("Execute the captured step: %v", err)
	}

	var gotPath string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer app.Close()
	rc.Config.Targets.App = &config.AppTarget{URL: app.URL}

	next := &scenario.Step{
		Name:   "read the inbox row for the captured message",
		Type:   "app/http",
		HTTP:   &scenario.HTTPSource{Method: http.MethodGet, Path: "/inbox/{{ .accepted.id }}/{{ .accepted.body.data.articleId }}"},
		Expect: &scenario.Expect{Status: 200},
	}

	result, err := Execute(context.Background(), next, rc)
	if err != nil {
		t.Fatalf("Execute the next step: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the next step failed: %v", result.Expect)
	}
	if gotPath != "/inbox/01ABCDEF/515150" {
		t.Errorf("request path = %q, want /inbox/01ABCDEF/515150", gotPath)
	}
}

// TestExecute_CapturedStepWithSeveralMatches_BindsTheFirst holds the ambiguity
// rule of SDD section 6.4.
func TestExecute_CapturedStepWithSeveralMatches_BindsTheFirst(t *testing.T) {
	second := capturedEntry()
	second["body"].(map[string]any)["id"] = "02FEDCBA"
	rc := capturedRunContext(t, []any{capturedEntry(), second})

	if _, err := Execute(context.Background(), capturedStep(), rc); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	binding, ok := rc.Bindings["accepted"].(map[string]any)
	if !ok {
		t.Fatalf("the binding is %T, want the map a template reads", rc.Bindings["accepted"])
	}
	if binding["id"] != "01ABCDEF" {
		t.Errorf("id = %v, want the first match", binding["id"])
	}
}
