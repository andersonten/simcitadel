package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the message references of SDD section 6.4. These tests
// began as a proof that a step was rendered when the scenario loaded, which is
// before any as: binding exists, so .accepted.id could never resolve.

// TestMessageRef_TemplateValue_UsesTheDocumentedNames holds the field spelling
// of SDD section 6.4. A Go template matches a map key exactly.
func TestMessageRef_TemplateValue_UsesTheDocumentedNames(t *testing.T) {
	ref := &scenario.MessageRef{
		ID:         "01ABC",
		Exchange:   "public.orchestration-engine.Article.v1",
		RoutingKey: "public.orchestration-engine.Article.v1",
		Body:       map[string]any{"data": map[string]any{"articleId": "a-1"}},
		Headers:    map[string]string{"eventType": "ArticleSubmitted"},
		SentAt:     "2026-08-19T09:00:00Z",
	}

	value := ref.TemplateValue()
	for _, key := range []string{"id", "exchange", "routingKey", "body", "headers", "sentAt"} {
		if _, ok := value[key]; !ok {
			t.Errorf("binding has no key %q", key)
		}
	}
	if _, unexpected := value["receivedAt"]; unexpected {
		t.Error("a sent message reports receivedAt")
	}
}

// TestMessageRef_TemplateValue_ReceivingStepReportsReceivedAt holds the second
// paragraph of SDD section 6.4.
func TestMessageRef_TemplateValue_ReceivingStepReportsReceivedAt(t *testing.T) {
	ref := &scenario.MessageRef{ID: "01ABC", ReceivedAt: "2026-08-19T09:00:01Z"}

	value := ref.TemplateValue()
	if value["receivedAt"] != "2026-08-19T09:00:01Z" {
		t.Errorf("receivedAt = %v, want the receive time", value["receivedAt"])
	}
	if _, unexpected := value["sentAt"]; unexpected {
		t.Error("a received message reports sentAt")
	}
}

// TestExecute_StepAfterABinding_ResolvesTheMessageReference is the point of the
// whole feature: a later step addresses what an earlier step bound.
func TestExecute_StepAfterABinding_ResolvesTheMessageReference(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	rc := &RunContext{
		Config:     &config.CitadelConfig{Targets: config.TargetsConfig{App: &config.AppTarget{URL: srv.URL}}},
		HTTPClient: srv.Client(),
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}

	// The publish step would bind this. Record it the way Execute does.
	rc.Bind("accepted", &scenario.MessageRef{
		ID:         "01ABCDEF",
		Exchange:   "public.a.v1",
		RoutingKey: "article-accepted",
		Body:       map[string]any{"data": map[string]any{"articleId": "a-42"}},
	})

	step := &scenario.Step{
		Name:   "read the article the publish step created",
		Type:   "app/http",
		HTTP:   &scenario.HTTPSource{Method: "GET", Path: "/articles/{{ .accepted.body.data.articleId }}"},
		Expect: &scenario.Expect{Status: 200},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("step failed: %v", result.Expect)
	}
	if gotPath != "/articles/a-42" {
		t.Errorf("request path = %q, want /articles/a-42", gotPath)
	}
}

// TestExecute_StepReferencingTheBindingID_ResolvesIt covers the flat fields of
// the binding as well as the nested body.
func TestExecute_StepReferencingTheBindingID_ResolvesIt(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	rc := &RunContext{
		Config:     &config.CitadelConfig{Targets: config.TargetsConfig{App: &config.AppTarget{URL: srv.URL}}},
		HTTPClient: srv.Client(),
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}
	rc.Bind("accepted", &scenario.MessageRef{ID: "01ABCDEF", RoutingKey: "article-accepted"})

	step := &scenario.Step{
		Name:   "read the inbox row for the message",
		Type:   "app/http",
		HTTP:   &scenario.HTTPSource{Method: "GET", Path: "/inbox/{{ .accepted.id }}"},
		Expect: &scenario.Expect{Status: 200},
	}

	if _, err := Execute(context.Background(), step, rc); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotPath != "/inbox/01ABCDEF" {
		t.Errorf("request path = %q, want /inbox/01ABCDEF", gotPath)
	}
}

// TestExecute_UnknownReference_IsAnError keeps a missing binding from rendering
// as an empty string, which SDD section 6.4 rules out.
func TestExecute_UnknownReference_IsAnError(t *testing.T) {
	rc := &RunContext{
		Config:    &config.CitadelConfig{Targets: config.TargetsConfig{App: &config.AppTarget{URL: "http://127.0.0.1:1"}}},
		Variables: map[string]any{},
		Bindings:  map[string]any{},
	}

	step := &scenario.Step{
		Name:   "reference a binding that was never made",
		Type:   "app/http",
		HTTP:   &scenario.HTTPSource{Method: "GET", Path: "/articles/{{ .missing.id }}"},
		Expect: &scenario.Expect{Status: 200},
	}

	_, err := Execute(context.Background(), step, rc)
	if err == nil {
		t.Fatal("an unresolved reference rendered without an error")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q does not name the unresolved reference", err.Error())
	}
}

// TestRenderStep_InlineData_IsTemplated holds SDD section 6.3, where an inline
// data payload is a first-class alternative to dataFile.
func TestRenderStep_InlineData_IsTemplated(t *testing.T) {
	step := &scenario.Step{
		Name:       "publish an inline payload",
		Type:       "event/publish",
		Exchange:   "public.a.v1",
		RoutingKey: "article-submitted",
		Data: map[string]any{
			"articleId": "{{ .articleId }}",
			"nested": map[string]any{
				"spaceId": "{{ .spaceId }}",
				"tags":    []any{"{{ .tag }}", "static"},
			},
			"count": 3,
		},
	}

	rendered, err := scenario.RenderStep(step, map[string]any{
		"articleId": "a-42",
		"spaceId":   "s-1",
		"tag":       "editorial",
	})
	if err != nil {
		t.Fatalf("RenderStep: %v", err)
	}

	data, ok := rendered.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is %T, want a map", rendered.Data)
	}
	if data["articleId"] != "a-42" {
		t.Errorf("articleId = %v, want a-42", data["articleId"])
	}
	nested, ok := data["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested is %T, want a map", data["nested"])
	}
	if nested["spaceId"] != "s-1" {
		t.Errorf("nested.spaceId = %v, want s-1", nested["spaceId"])
	}
	tags, ok := nested["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Fatalf("nested.tags = %v, want two entries", nested["tags"])
	}
	if tags[0] != "editorial" || tags[1] != "static" {
		t.Errorf("nested.tags = %v, want [editorial static]", tags)
	}
	if data["count"] != 3 {
		t.Errorf("count = %v, want the untouched number 3", data["count"])
	}
}
