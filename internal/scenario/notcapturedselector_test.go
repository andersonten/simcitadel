package scenario

import (
	"strings"
	"testing"
)

// notCapturedScenario wraps one event/not-captured step in a scenario that is
// otherwise complete.
func notCapturedScenario(step Step) *Scenario {
	step.Type = "event/not-captured"
	step.Name = "no-duplicate"
	return &Scenario{
		Name:       "s",
		Assertions: []Step{step},
	}
}

// selectorErrors returns only the errors that complain about a missing
// selector, so an unrelated check cannot make the test pass or fail.
func selectorErrors(errs []error) []string {
	var out []string
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires at least one selector") {
			out = append(out, e.Error())
		}
	}
	return out
}

// TestValidate_NotCapturedSelectorUnderCaptured_IsAccepted proves the validator
// agrees with the runtime. The executor reads every selector from the step level
// and from the captured block, so a scenario that writes one only under
// captured: is valid and must not fail check 2 of SDD section 13.
func TestValidate_NotCapturedSelectorUnderCaptured_IsAccepted(t *testing.T) {
	cases := map[string]Step{
		"exchange":         {Captured: &CapturedQuery{Exchange: "public.orchestration-engine.Article.v1"}},
		"routingKey":       {Captured: &CapturedQuery{RoutingKey: "public.orchestration-engine.Article.v1"}},
		"eventType":        {Captured: &CapturedQuery{EventType: "EditorAssigned"}},
		"eventTypePattern": {Captured: &CapturedQuery{EventTypePattern: "Editor.*"}},
		"headers":          {Captured: &CapturedQuery{Headers: map[string]string{"aggregateId": "515150"}}},

		// These two already passed. They are here so a change cannot lose them.
		"routingKeyPattern": {Captured: &CapturedQuery{RoutingKeyPattern: "public.*"}},
		"jsonPath":          {Captured: &CapturedQuery{JSONPath: "$.data.articleId"}},
	}

	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			errs := Validate(notCapturedScenario(step), "")
			if found := selectorErrors(errs); len(found) > 0 {
				t.Errorf("the runtime applies captured.%s, and validation rejects it: %v", name, found)
			}
		})
	}
}

// TestValidate_NotCapturedSelectorAtStepLevel_IsAccepted keeps the step-level
// shape working, because a scenario can write either.
func TestValidate_NotCapturedSelectorAtStepLevel_IsAccepted(t *testing.T) {
	cases := map[string]Step{
		"exchange":         {Exchange: "public.orchestration-engine.Article.v1"},
		"routingKey":       {RoutingKey: "public.orchestration-engine.Article.v1"},
		"eventType":        {EventType: "EditorAssigned"},
		"eventTypePattern": {EventTypePattern: "Editor.*"},
		"headerFilter":     {HeaderFilter: map[string]string{"aggregateId": "515150"}},
	}

	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			if found := selectorErrors(Validate(notCapturedScenario(step), "")); len(found) > 0 {
				t.Errorf("a step-level %s must stay valid: %v", name, found)
			}
		})
	}
}

// TestValidate_NotCapturedWithNoSelector_IsRejected keeps the check meaningful.
// A negative assertion with no selector matches every message, so it can never
// pass, and saying so at validate time is the whole point of check 2.
func TestValidate_NotCapturedWithNoSelector_IsRejected(t *testing.T) {
	if found := selectorErrors(Validate(notCapturedScenario(Step{}), "")); len(found) == 0 {
		t.Error("event/not-captured with no selector at all must be rejected")
	}
}

// TestValidate_NotCapturedWithAnEmptyCapturedBlock_IsRejected covers the block
// that exists and names nothing. A present but empty block is not a selector.
func TestValidate_NotCapturedWithAnEmptyCapturedBlock_IsRejected(t *testing.T) {
	step := Step{Captured: &CapturedQuery{}}
	if found := selectorErrors(Validate(notCapturedScenario(step), "")); len(found) == 0 {
		t.Error("an empty captured block is not a selector, so the step must be rejected")
	}
}

// TestValidate_NotCapturedWithOnlyRunID_IsRejected covers the one captured
// field that is not a selector. A run ID scopes a query and narrows nothing
// about which message the assertion forbids.
func TestValidate_NotCapturedWithOnlyRunID_IsRejected(t *testing.T) {
	step := Step{Captured: &CapturedQuery{RunID: "0191c3"}}
	if found := selectorErrors(Validate(notCapturedScenario(step), "")); len(found) == 0 {
		t.Error("a run ID is not a selector, so the step must be rejected")
	}
}
