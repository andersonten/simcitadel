package scenario

import (
	"strings"
	"testing"
)

// TestRenderStep_EveryCapturedSelector_IsRendered proves that every selector of
// the captured block goes through the template engine of SDD section 6.4. An
// unrendered selector reaches the capture service as the literal text of the
// template, the filter matches nothing, and a negative assertion then passes for
// the wrong reason. A false pass is worse than a failure.
func TestRenderStep_EveryCapturedSelector_IsRendered(t *testing.T) {
	vars := map[string]any{"articleId": "515098", "eventName": "EditorAssigned"}

	step := &Step{
		Name: "assert-no-duplicate",
		Type: "event/not-captured",
		Captured: &CapturedQuery{
			Exchange:          "public.{{ .articleId }}.v1",
			RoutingKey:        "rk.{{ .articleId }}",
			RoutingKeyPattern: "public.*.{{ .articleId }}",
			EventType:         "{{ .eventName }}",
			EventTypePattern:  "{{ .eventName }}.*",
			RunID:             "{{ .articleId }}",
			JSONPath:          "$.data[?(@.id=='{{ .articleId }}')]",
			Headers:           map[string]string{"aggregateId": "{{ .articleId }}"},
		},
	}

	rendered, err := RenderStep(step, vars)
	if err != nil {
		t.Fatalf("render the step: %v", err)
	}

	c := rendered.Captured
	fields := map[string]string{
		"exchange":             c.Exchange,
		"routingKey":           c.RoutingKey,
		"routingKeyPattern":    c.RoutingKeyPattern,
		"eventType":            c.EventType,
		"eventTypePattern":     c.EventTypePattern,
		"runId":                c.RunID,
		"jsonPath":             c.JSONPath,
		"headers[aggregateId]": c.Headers["aggregateId"],
	}
	for name, value := range fields {
		if strings.Contains(value, "{{") {
			t.Errorf("captured.%s is %q, which still holds template text", name, value)
		}
		if !strings.Contains(value, "515098") && !strings.Contains(value, "EditorAssigned") {
			t.Errorf("captured.%s is %q, and it resolved no variable", name, value)
		}
	}
}

// TestRenderStep_CapturedHeaders_DoNotShareTheOriginalMap proves the render
// writes a new header map. A shared map would let one step overwrite the
// scenario as it was loaded, so a retry of a later step would read a value that
// an earlier step rendered.
func TestRenderStep_CapturedHeaders_DoNotShareTheOriginalMap(t *testing.T) {
	step := &Step{
		Name:     "assert",
		Type:     "event/not-captured",
		Captured: &CapturedQuery{Headers: map[string]string{"aggregateId": "{{ .articleId }}"}},
	}

	rendered, err := RenderStep(step, map[string]any{"articleId": "515098"})
	if err != nil {
		t.Fatalf("render the step: %v", err)
	}

	if got := step.Captured.Headers["aggregateId"]; got != "{{ .articleId }}" {
		t.Errorf("the original step now holds %q, so the render changed the scenario as it was loaded", got)
	}
	if got := rendered.Captured.Headers["aggregateId"]; got != "515098" {
		t.Errorf("the rendered header is %q, want 515098", got)
	}
}

// TestRenderStep_CapturedWithoutHeaders_StaysNil keeps a step that names no
// header free of an empty map, so the query that reaches the capture service
// carries no header filter at all.
func TestRenderStep_CapturedWithoutHeaders_StaysNil(t *testing.T) {
	step := &Step{
		Name:     "assert",
		Type:     "event/not-captured",
		Captured: &CapturedQuery{Exchange: "public.x.v1"},
	}

	rendered, err := RenderStep(step, map[string]any{})
	if err != nil {
		t.Fatalf("render the step: %v", err)
	}
	if rendered.Captured.Headers != nil {
		t.Errorf("headers is %v, want nil for a step that names none", rendered.Captured.Headers)
	}
}
