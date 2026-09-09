package scenario

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// This file locks down three defects of the checks of SDD section 13. Check 9
// accepted an unknown state, check 4 ignored an inline payload, and check 6 let
// a probe in the steps phase point at WireMock.

// writeStub writes one WireMock stub file into a fresh directory and returns
// the directory.
func writeStub(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return dir
}

// setStateScenario returns a scenario with one wiremock/set-state step.
func setStateScenario(scenarioName, state string) *Scenario {
	return &Scenario{
		Name: "set-state",
		Steps: []Step{{
			Name:         "move the mock",
			Type:         "wiremock/set-state",
			ScenarioName: scenarioName,
			State:        state,
		}},
	}
}

// TestValidate_SetStateUnknownState_IsRejected holds check 9 of SDD section 13.
// A typo in the state name moves the mock to a state no stub requires, and
// every later stub then misses for a reason the output does not show.
func TestValidate_SetStateUnknownState_IsRejected(t *testing.T) {
	dir := writeStub(t, `{
		"scenarioName": "ArticleLifecycle",
		"requiredScenarioState": "ArticleSubmitted",
		"newScenarioState": "ArticleAccepted"
	}`)

	errs := Validate(setStateScenario("ArticleLifecycle", "ArticleSubmited"), dir)
	if !hasErrorContaining(errs, `state "ArticleSubmited" does not appear in any stub`) {
		t.Errorf("an unknown state was accepted; errors: %v", errs)
	}
}

// TestValidate_SetStateWrongCaseState_IsRejected holds the case-sensitive match
// that check 9 of SDD section 13 requires.
func TestValidate_SetStateWrongCaseState_IsRejected(t *testing.T) {
	dir := writeStub(t, `{
		"scenarioName": "ArticleLifecycle",
		"requiredScenarioState": "ArticleSubmitted"
	}`)

	errs := Validate(setStateScenario("ArticleLifecycle", "articlesubmitted"), dir)
	if !hasErrorContaining(errs, `state "articlesubmitted" does not appear in any stub`) {
		t.Errorf("a state with the wrong case was accepted; errors: %v", errs)
	}
}

// TestValidate_SetStateKnownState_IsAccepted keeps check 9 from rejecting a
// state that a stub declares. A required state and a new state both count.
func TestValidate_SetStateKnownState_IsAccepted(t *testing.T) {
	dir := writeStub(t, `{
		"scenarioName": "ArticleLifecycle",
		"requiredScenarioState": "ArticleSubmitted",
		"newScenarioState": "ArticleAccepted"
	}`)

	for _, state := range []string{"ArticleSubmitted", "ArticleAccepted"} {
		t.Run(state, func(t *testing.T) {
			if errs := Validate(setStateScenario("ArticleLifecycle", state), dir); len(errs) != 0 {
				t.Errorf("a declared state was rejected: %v", errs)
			}
		})
	}
}

// TestValidate_SetStateWithoutStubFiles_IsAccepted keeps a scenario that runs
// without a WireMock pack valid. The check stays silent with no stub file.
func TestValidate_SetStateWithoutStubFiles_IsAccepted(t *testing.T) {
	dir := t.TempDir()

	if errs := Validate(setStateScenario("ArticleLifecycle", "AnyState"), dir); len(errs) != 0 {
		t.Errorf("a set-state step without a stub pack was rejected: %v", errs)
	}
}

// TestValidate_InlineDataUnknownTemplateRef_IsRejected holds check 4 of SDD
// section 13 for the inline payload of SDD section 6.4. The renderer walks the
// map, so a reference inside it is a real reference.
func TestValidate_InlineDataUnknownTemplateRef_IsRejected(t *testing.T) {
	s := &Scenario{
		Name:      "inline-data-ref",
		Variables: map[string]any{"articleId": "515098"},
		Steps: []Step{{
			Name:       "publish an inline payload",
			Type:       "event/publish",
			Exchange:   "Journal.Article",
			RoutingKey: "Journal.Article-Submitted.1.0.0",
			Data: map[string]any{
				"data": map[string]any{
					"articleId": "{{ .articleId }}",
					"authors": []any{
						map[string]any{"name": "{{ .doesNotExist }}"},
					},
				},
			},
		}},
	}

	errs := Validate(s, "")
	if !hasErrorContaining(errs, "template reference .doesNotExist") {
		t.Errorf("an unresolved reference inside data was accepted; errors: %v", errs)
	}
}

// TestValidate_InlineDataKnownTemplateRef_IsAccepted keeps check 4 from
// rejecting an inline payload that the variables block resolves.
func TestValidate_InlineDataKnownTemplateRef_IsAccepted(t *testing.T) {
	s := &Scenario{
		Name:      "inline-data-ok",
		Variables: map[string]any{"articleId": "515098"},
		Steps: []Step{{
			Name:       "publish an inline payload",
			Type:       "event/publish",
			Exchange:   "Journal.Article",
			RoutingKey: "Journal.Article-Submitted.1.0.0",
			Data: map[string]any{
				"data":  map[string]any{"articleId": "{{ .articleId }}"},
				"count": 3,
			},
		}},
	}

	if errs := Validate(s, ""); len(errs) != 0 {
		t.Errorf("a resolved reference inside data was rejected: %v", errs)
	}
}

// TestValidate_NestedSourceUnknownTemplateRef_IsRejected closes the same hole
// for a graphql variable and an http body that hold a nested value.
func TestValidate_NestedSourceUnknownTemplateRef_IsRejected(t *testing.T) {
	cases := []struct {
		name string
		step Step
	}{
		{
			name: "graphql variables",
			step: Step{
				Name: "query with a nested variable",
				Type: "app/graphql",
				GraphQL: &GraphQLSource{
					Query: "query Q($filter: Filter) { x }",
					Variables: map[string]any{
						"filter": map[string]any{"id": "{{ .doesNotExist }}"},
					},
				},
				Expect: &Expect{Status: 200},
			},
		},
		{
			name: "http body",
			step: Step{
				Name: "post a nested body",
				Type: "app/http",
				HTTP: &HTTPSource{
					Method: "POST",
					Path:   "/articles",
					Body:   map[string]any{"article": map[string]any{"id": "{{ .doesNotExist }}"}},
				},
				Expect: &Expect{Status: 200},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate(&Scenario{Name: "nested-refs", Steps: []Step{tc.step}}, "")
			if !hasErrorContaining(errs, "template reference .doesNotExist") {
				t.Errorf("an unresolved nested reference was accepted; errors: %v", errs)
			}
		})
	}
}

// TestRenderStep_NestedSourceValue_ResolvesTheReference keeps the renderer and
// check 4 in agreement. A reference that validate accepts must render.
func TestRenderStep_NestedSourceValue_ResolvesTheReference(t *testing.T) {
	step := &Step{
		Name: "nested render",
		Type: "app/http",
		HTTP: &HTTPSource{
			Method: "POST",
			Path:   "/articles",
			Body:   map[string]any{"article": map[string]any{"id": "{{ .articleId }}"}},
		},
		GraphQL: &GraphQLSource{
			Variables: map[string]any{
				"filter": map[string]any{"ids": []any{"{{ .articleId }}"}},
			},
		},
	}

	out, err := RenderStep(step, map[string]any{"articleId": "515098"})
	if err != nil {
		t.Fatalf("render step: %v", err)
	}

	body, ok := out.HTTP.Body.(map[string]any)
	if !ok {
		t.Fatalf("http body is %T, want a map", out.HTTP.Body)
	}
	article, ok := body["article"].(map[string]any)
	if !ok {
		t.Fatalf("http body article is %T, want a map", body["article"])
	}
	if article["id"] != "515098" {
		t.Errorf("http body id is %v, want 515098", article["id"])
	}

	filter, ok := out.GraphQL.Variables["filter"].(map[string]any)
	if !ok {
		t.Fatalf("graphql filter is %T, want a map", out.GraphQL.Variables["filter"])
	}
	ids, ok := filter["ids"].([]any)
	if !ok {
		t.Fatalf("graphql filter ids is %T, want a list", filter["ids"])
	}
	if len(ids) != 1 || ids[0] != "515098" {
		t.Errorf("graphql filter ids is %v, want [515098]", ids)
	}
}

// TestValidate_ProbeInStepsPhaseTargetingWireMock_IsRejected holds check 6 of
// SDD section 13 in the steps phase. A probe against the mock proves nothing
// about the application, whichever list holds it.
func TestValidate_ProbeInStepsPhaseTargetingWireMock_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "probe-the-mock-in-steps",
		Steps: []Step{{
			Name:    "read the mock and call it the app",
			Type:    "probe",
			Target:  "wiremock",
			GraphQL: &GraphQLSource{Query: "{ __typename }"},
			Expect:  &Expect{Status: 200},
		}},
	}

	errs := Validate(s, "")
	if !hasErrorContaining(errs, "would assert against the mock") {
		t.Errorf("a probe against WireMock in the steps phase was accepted; errors: %v", errs)
	}
}

// TestValidate_ProbeInStepsPhaseTargetingApp_IsAccepted keeps check 6 from
// rejecting the documented probe of SDD section 6.5.
func TestValidate_ProbeInStepsPhaseTargetingApp_IsAccepted(t *testing.T) {
	s := &Scenario{
		Name: "probe-the-app-in-steps",
		Steps: []Step{{
			Name:    "read the app",
			Type:    "probe",
			Target:  "app",
			GraphQL: &GraphQLSource{Query: "{ __typename }"},
			Expect:  &Expect{Status: 200},
		}},
	}

	if errs := Validate(s, ""); len(errs) != 0 {
		t.Errorf("a probe against the app was rejected: %v", errs)
	}
}

// TestValidateWithOptions_ProbeTargetingWireMockAlias_IsRejected holds the
// resolved half of check 6. The check compares addresses, so a target with
// another name that points at the mock cannot defeat it.
func TestValidateWithOptions_ProbeTargetingWireMockAlias_IsRejected(t *testing.T) {
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{
				URL:      "http://127.0.0.1:9090",
				AdminURL: "http://127.0.0.1:9090/__admin",
			},
			App: &config.AppTarget{URL: "http://127.0.0.1:5000", GraphQLPath: "/graphql"},
			// The event trigger is named separately and points at the mock.
			EventTrigger: &config.EventTriggerTarget{URL: "http://127.0.0.1:9090"},
		},
	}

	s := &Scenario{
		Name: "probe-an-alias-of-the-mock",
		Steps: []Step{{
			Name:   "read the alias",
			Type:   "probe",
			Target: "event-trigger",
			HTTP:   &HTTPSource{Method: "GET", Path: "/articles/1"},
			Expect: &Expect{Status: 200},
		}},
	}

	errs := ValidateWithOptions(s, Options{Config: cfg})
	if !hasErrorContaining(errs, "would assert against the mock") {
		t.Errorf("a probe against an alias of WireMock was accepted; errors: %v", errs)
	}
}
