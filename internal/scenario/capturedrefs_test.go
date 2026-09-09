package scenario

import (
	"strings"
	"testing"
)

// This file holds check 4 of SDD section 13 against every field of a captured
// block.

// TestValidateTemplateRefs_CapturedPatternFields_AreChecked holds check 4 of
// SDD section 13 to every field of a captured block. The validator once read
// captured.exchange, routingKey, eventType, runId and jsonPath, and it skipped
// routingKeyPattern, eventTypePattern and headers, so a scenario with an
// undefined variable in a skipped field passed citadel validate and then
// aborted at run time. See templatecoverage_test.go for the gate that keeps
// the renderer and the validator level with the Step type.
func TestValidateTemplateRefs_CapturedPatternFields_AreChecked(t *testing.T) {
	cases := []struct {
		field string
		step  Step
	}{
		{
			field: "captured.routingKey",
			step: Step{
				Name:     "control: a checked field",
				Type:     "event/captured",
				Captured: &CapturedQuery{RoutingKey: "{{ .undefinedVariable }}"},
			},
		},
		{
			field: "captured.routingKeyPattern",
			step: Step{
				Name:     "the manuscript event reached the broker",
				Type:     "event/captured",
				Captured: &CapturedQuery{RoutingKeyPattern: "{{ .undefinedVariable }}.#"},
			},
		},
		{
			field: "captured.eventTypePattern",
			step: Step{
				Name:     "the manuscript event reached the broker",
				Type:     "event/captured",
				Captured: &CapturedQuery{EventTypePattern: "{{ .undefinedVariable }}*"},
			},
		},
		{
			field: "captured.headers",
			step: Step{
				Name: "the manuscript event reached the broker",
				Type: "event/captured",
				Captured: &CapturedQuery{
					Exchange: "public.orchestration-engine.Manuscript.v1",
					Headers:  map[string]string{"aggregateId": "{{ .undefinedVariable }}"},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			s := &Scenario{
				Name:      "review",
				Variables: map[string]any{},
				Steps:     []Step{tc.step},
			}

			// The renderer reads the field, so the field is templated.
			rendered, err := RenderStep(&tc.step, map[string]any{})
			if err == nil {
				t.Fatalf("render of %s did not fail on an undefined variable: %+v",
					tc.field, rendered.Captured)
			}

			// Check 4 must report the same reference before the run starts.
			errs := validateTemplateRefs(s)
			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), "undefinedVariable") {
					found = true
				}
			}
			if !found {
				t.Errorf("check 4 accepted an undefined template reference in %s; "+
					"validate reported %d errors", tc.field, len(errs))
			}
		})
	}
}
