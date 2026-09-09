package scenario

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// This file holds the validation rules of SDD section 6, section 13 and
// section 14.3: the closed step-type set, the WireMock pack checks, and the
// template dialect limits.

// documentedLayout writes a project in the layout of SDD section 5.1: a config
// file at the root, and one directory for each content kind. The WireMock pack
// holds one stub, which declares the scenario ArticleLifecycle and the two
// states ArticleSubmitted and ArticleAssigned.
//
// The function returns the loaded config, the scenario directory, and the
// project root. The scenario directory is what both call sites of
// ValidateWithOptions pass as the base path, so a test that uses it reproduces
// what a developer gets from the command line.
func documentedLayout(t *testing.T) (*config.CitadelConfig, string, string) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"scenarios", "payloads", "sql", "queries", filepath.Join("wiremock", "mappings")} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	configBody := `version: 1
environment:
  name: local
targets:
  wiremock:
    url: http://127.0.0.1:9090
    adminUrl: http://127.0.0.1:9090/__admin
  app:
    url: http://127.0.0.1:5000
paths:
  scenarios: ./scenarios
  payloads:  ./payloads
  sql:       ./sql
  queries:   ./queries
  wiremock:  ./wiremock
`
	configPath := filepath.Join(root, "citadel.yaml")
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stub := `{"mappings":[
	  {"scenarioName":"ArticleLifecycle","requiredScenarioState":"ArticleSubmitted","newScenarioState":"ArticleAssigned",
	   "request":{"method":"POST","url":"/graphql"},"response":{"status":200}}
	]}`
	stubPath := filepath.Join(root, "wiremock", "mappings", "article.json")
	if err := os.WriteFile(stubPath, []byte(stub), 0o600); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg, filepath.Join(root, "scenarios"), root
}

// TestValidateWithOptions_SetStateAgainstTheConfiguredPack_IsChecked holds check
// 9 of SDD section 13 at the base path both call sites supply. Check 9 is one of
// the checks section 13 calls the most important, so it must fire for the layout
// of section 5.1 and not only for a base path that happens to sit above the
// pack.
func TestValidateWithOptions_SetStateAgainstTheConfiguredPack_IsChecked(t *testing.T) {
	cfg, scenarioDir, _ := documentedLayout(t)

	errs := ValidateWithOptions(setStateScenario("ArticleLifecycle", "ArticleWithdrawn"),
		Options{BasePath: scenarioDir, Config: cfg})

	if !hasErrorContaining(errs, `state "ArticleWithdrawn" does not appear in any stub`) {
		t.Errorf("check 9 did not fire for a state no stub declares; errors: %v", errs)
	}
}

// TestValidateWithOptions_SetStateNamedInAPayloadFile_IsRejected states the
// second half of the same defect. Check 9 requires the state to appear in a
// stub file, and a payload file is not a stub file.
func TestValidateWithOptions_SetStateNamedInAPayloadFile_IsRejected(t *testing.T) {
	cfg, _, root := documentedLayout(t)

	payload := `{"scenarioName":"ArticleLifecycle","requiredScenarioState":"ArticleWithdrawn"}`
	if err := os.WriteFile(filepath.Join(root, "payloads", "decoy.json"), []byte(payload), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	errs := ValidateWithOptions(setStateScenario("ArticleLifecycle", "ArticleWithdrawn"),
		Options{BasePath: root, Config: cfg})

	if !hasErrorContaining(errs, `state "ArticleWithdrawn" does not appear in any stub`) {
		t.Errorf("a state declared only in a payload file satisfied check 9; errors: %v", errs)
	}
}

// TestValidateWithOptions_WebhookGuardReadsOnlyTheWireMockPack holds check 8 to
// the same directory. A payload file is not a stub file, so a postServeActions
// key inside one is not a webhook and must not fail the scenario.
func TestValidateWithOptions_WebhookGuardReadsOnlyTheWireMockPack(t *testing.T) {
	cfg, _, root := documentedLayout(t)

	payload := `{"postServeActions":[{"name":"webhook"}],"articleId":"515098"}`
	if err := os.WriteFile(filepath.Join(root, "payloads", "event.json"), []byte(payload), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	errs := ValidateWithOptions(setStateScenario("ArticleLifecycle", "ArticleSubmitted"),
		Options{BasePath: root, Config: cfg})

	if hasErrorContaining(errs, "postServeActions") {
		t.Errorf("check 8 read a payload file as a WireMock stub; errors: %v", errs)
	}
}

// TestRenderString_TrimMarkedControlFlow_IsRejected holds constraint C4 of SDD
// section 6.4: there is no conditional and no loop. A trim marker is legal Go
// template syntax, so a guard that reads the two braces and the keyword next to
// them lets the whole construct through.
func TestRenderString_TrimMarkedControlFlow_IsRejected(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
	}{
		{"trim marked if", "{{- if .x }}yes{{- end }}"},
		{"trim marked range", "{{- range .items }}item{{- end }}"},
		{"trim marked with", "{{- with .x }}val{{- end }}"},
		{"right trim marked if", "{{ if .x -}}yes{{ end -}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]any{"x": "1", "items": []string{"a"}}
			out, err := RenderString(tc.tmpl, vars)
			if err == nil {
				t.Fatalf("a conditional rendered to %q instead of failing", out)
			}
			if !strings.Contains(err.Error(), "forbidden directive") {
				t.Fatalf("error %q does not name the forbidden directive", err.Error())
			}
		})
	}
}

// TestRenderString_PipedBuiltin_IsRejected holds the same constraint for a
// built-in that arrives second in a pipeline. SDD section 6.4 gives four
// functions and no comparison, and a pipeline reaches the same built-in as a
// leading call does.
func TestRenderString_PipedBuiltin_IsRejected(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
	}{
		{"piped printf", `{{ .a | printf "%s" }}`},
		{"piped len", "{{ .a | len }}"},
		{"piped comparison", "{{ .a | eq .b }}"},
		{"piped not", "{{ .a | not }}"},
		{"variable declaration", "{{ $v := .a }}{{ $v }}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]any{"a": "1", "b": "2"}
			out, err := RenderString(tc.tmpl, vars)
			if err == nil {
				t.Fatalf("a built-in rendered to %q instead of failing", out)
			}
			if !strings.Contains(err.Error(), "forbidden directive") {
				t.Fatalf("error %q does not name the forbidden directive", err.Error())
			}
		})
	}
}

// TestValidate_ProbeWithTwoSources_IsRejected holds the first sentence of SDD
// section 6.5: a probe has exactly one source. The executor reads one of them,
// so the second is silently dropped and the assertion reports on something the
// author did not write.
func TestValidate_ProbeWithTwoSources_IsRejected(t *testing.T) {
	cases := []struct {
		name string
		step Step
	}{
		{
			name: "graphql and captured",
			step: Step{
				Name:     "read the task list",
				Type:     "await",
				GraphQL:  &GraphQLSource{Query: "query { pe_tasks { status } }"},
				Captured: &CapturedQuery{Exchange: "public.orchestration-engine.Task.v1"},
				Expect:   &Expect{CountAtLeast: intPointer(1)},
			},
		},
		{
			name: "http and sql",
			step: Step{
				Name:   "read the manuscript",
				Type:   "probe",
				HTTP:   &HTTPSource{Method: "GET", Path: "/manuscripts/1"},
				SQL:    &SQLSource{Query: "SELECT 1"},
				Expect: &Expect{Status: 200},
			},
		},
		{
			name: "captured and journal",
			step: Step{
				Name:     "read the journal",
				Type:     "probe",
				Captured: &CapturedQuery{Exchange: "public.orchestration-engine.Task.v1"},
				Journal:  &JournalQuery{Method: "POST", URLPath: "/graphql"},
				Expect:   &Expect{Count: intPointer(1)},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scenario{Name: "two-sources", Steps: []Step{tc.step}}
			errs := Validate(s, "")
			if !hasErrorContaining(errs, "exactly one source") {
				t.Errorf("a probe with two sources passed validation; errors: %v", errs)
			}
		})
	}
}

// TestValidate_ExpectWithNoExpectationKey_IsRejected holds the expectation table
// of SDD section 6.5, which is the complete list of keys. A block that names no
// key from it asserts nothing, and the evaluator reports a pass, so a failing
// system reports a pass.
func TestValidate_ExpectWithNoExpectationKey_IsRejected(t *testing.T) {
	cases := []struct {
		name   string
		expect *Expect
	}{
		{"empty block", &Expect{}},
		{"equals with no jsonPath", &Expect{Equals: "OPEN"}},
		{"equals with a jsonPath map", &Expect{JSONPath: map[string]any{"$.a": 1}, Equals: "OPEN"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scenario{
				Name: "hollow-expectation",
				Steps: []Step{{
					Name:    "read the task list",
					Type:    "await",
					GraphQL: &GraphQLSource{Query: "query { pe_tasks { status } }"},
					Expect:  tc.expect,
				}},
			}
			errs := Validate(s, "")
			if !hasErrorContaining(errs, "asserts nothing") {
				t.Errorf("an expectation that asserts nothing passed validation; errors: %v", errs)
			}
		})
	}
}

// TestValidate_GraphQLErrorsOutsideTheClosedSet_IsRejected holds the last row of
// the expectation table of SDD section 6.5, which gives graphqlErrors two
// values. A third value reads as the strict default, so a scenario that means
// any silently gets none.
func TestValidate_GraphQLErrorsOutsideTheClosedSet_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "typed-the-wrong-word",
		Steps: []Step{{
			Name:    "read the task list",
			Type:    "app/graphql",
			GraphQL: &GraphQLSource{Query: "query { pe_tasks { status } }"},
			Expect:  &Expect{Status: 200, GraphQLErrors: "None"},
		}},
	}

	errs := Validate(s, "")
	if !hasErrorContaining(errs, "graphqlErrors") {
		t.Errorf("graphqlErrors accepted a value outside none and any; errors: %v", errs)
	}
}

// TestValidate_BindingFieldOutsideTheTable_IsRejected holds the message
// reference table of SDD section 6.4. An unresolved reference is an error at
// validate time, and a field the table does not name resolves to nothing.
func TestValidate_BindingFieldOutsideTheTable_IsRejected(t *testing.T) {
	cases := []struct {
		name      string
		bindType  string
		reference string
		want      string
	}{
		{"undocumented field", "event/publish", "{{ .submitted.messageId }}",
			`names "messageId", which is not a field of the message binding "submitted"`},
		{"sentAt on a received message", "event/captured", "{{ .submitted.sentAt }}",
			`names "sentAt", which is not a field of the message binding "submitted"`},
		{"depth under a scalar field", "event/publish", "{{ .submitted.id.value }}",
			`reads through "id", which holds a single value`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bind := Step{
				Name:       "bind the message",
				Type:       tc.bindType,
				Exchange:   "public.orchestration-engine.Article.v1",
				RoutingKey: "public.orchestration-engine.Article.v1",
				Data:       map[string]any{"articleId": "515098"},
				As:         "submitted",
			}
			if tc.bindType == "event/captured" {
				bind.Captured = &CapturedQuery{Exchange: "public.orchestration-engine.Article.v1"}
				bind.Expect = &Expect{CountAtLeast: intPointer(1)}
			}
			s := &Scenario{
				Name:  "bad-binding-field",
				Steps: []Step{bind, {Name: "use it", Type: "wait", Duration: "1s", Reason: tc.reference}},
			}
			errs := Validate(s, "")
			if !hasErrorContaining(errs, tc.want) {
				t.Errorf("a reference to a field the table does not name passed validation; errors: %v", errs)
			}
		})
	}
}

// TestValidate_DocumentedBindingFields_AreAccepted is the positive half, so the
// test above cannot pass because every reference is rejected.
func TestValidate_DocumentedBindingFields_AreAccepted(t *testing.T) {
	references := []string{
		"{{ .submitted.id }}",
		"{{ .submitted.exchange }}",
		"{{ .submitted.routingKey }}",
		"{{ .submitted.sentAt }}",
		"{{ .submitted.body.data.articleId }}",
		"{{ .submitted.headers.eventType }}",
		"{{ .submitted }}",
	}
	for _, ref := range references {
		t.Run(ref, func(t *testing.T) {
			s := &Scenario{
				Name: "good-binding-field",
				Steps: []Step{
					{
						Name:       "bind the message",
						Type:       "event/publish",
						Exchange:   "public.orchestration-engine.Article.v1",
						RoutingKey: "public.orchestration-engine.Article.v1",
						Data:       map[string]any{"articleId": "515098"},
						As:         "submitted",
					},
					{Name: "use it", Type: "wait", Duration: "1s", Reason: ref},
				},
			}
			for _, err := range Validate(s, "") {
				t.Errorf("a documented binding field was rejected: %v", err)
			}
		})
	}
}

// TestValidate_RepublishWithoutRef_IsRejected holds check 2 of SDD section 13
// for the one field an event/republish cannot work without. Section 6.2 makes
// the step re-send a stored message, and ref: is the only way to name that
// message.
func TestValidate_RepublishWithoutRef_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "republish-nothing",
		Steps: []Step{{
			Name:       "send the message again",
			Type:       "event/republish",
			Exchange:   "public.orchestration-engine.Article.v1",
			RoutingKey: "public.orchestration-engine.Article.v1",
		}},
	}

	errs := Validate(s, "")
	if !hasErrorContaining(errs, "event/republish requires ref") {
		t.Errorf("a republish with nothing to re-send passed validation; errors: %v", errs)
	}
}

// stepTypeTableRow matches one row of the step type table of SDD section 6.2.
var stepTypeTableRow = regexp.MustCompile("^\\|\\s*`([^`]+)`\\s*\\|")

// TestStepTypeRegistry_MatchesTheDocumentedClosedSet is constraint test C4 of
// SDD section 14.3. It reads the closed set out of the step type table of
// section 6.2, so a type that reaches the registry without a row in the table
// fails the build, and a row with no registry entry fails it too.
func TestStepTypeRegistry_MatchesTheDocumentedClosedSet(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "docs", "sdd.md"))
	if err != nil {
		t.Fatalf("read the design document: %v", err)
	}

	documented := make(map[string]bool)
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "### 6.2 ") {
			inSection = true
			continue
		}
		if inSection && strings.HasPrefix(line, "### ") {
			break
		}
		if !inSection {
			continue
		}
		m := stepTypeTableRow.FindStringSubmatch(line)
		if m == nil || m[1] == "type" {
			continue
		}
		documented[m[1]] = true
	}

	if len(documented) == 0 {
		t.Fatal("the step type table of section 6.2 yielded no row")
	}

	for name := range documented {
		if !validStepTypes[name] {
			t.Errorf("section 6.2 documents step type %q, which the registry does not accept", name)
		}
	}
	for name := range validStepTypes {
		if !documented[name] {
			t.Errorf("the registry accepts step type %q, which section 6.2 does not document", name)
		}
	}
}

// intPointer returns a pointer to an int literal.
func intPointer(v int) *int { return &v }

// TestValidate_UnreadableQuietPeriod_IsRejected holds the per-step quietPeriod
// of SDD section 8.3 to a duration. Validate already checks timeout,
// retry.timeout and retry.interval, and this field reached the executor
// unchecked, where an unreadable value took the config default and shortened
// the window the assertion depends on.
func TestValidate_UnreadableQuietPeriod_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "quiet-period",
		Steps: []Step{{
			Name:        "no reminder is sent",
			Type:        "event/not-captured",
			Exchange:    "public.orchestration-engine.Article.v1",
			QuietPeriod: "30 seconds",
		}},
	}

	errs := Validate(s, "")
	if !hasErrorContaining(errs, "quietPeriod") {
		t.Errorf("an unreadable quietPeriod passed validation; errors: %v", errs)
	}
}

// TestValidate_UnreadableWaitDuration_IsRejected holds the duration of a wait
// step. SDD section 6.2 keeps wait in the closed set as a deliberate friction,
// and a value that does not parse must fail before the run rather than during
// it.
func TestValidate_UnreadableWaitDuration_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "wait-duration",
		Steps: []Step{{
			Name:     "let the scheduler tick",
			Type:     "wait",
			Duration: "2 minutes",
			Reason:   "the scheduler has no readable state to poll",
		}},
	}

	errs := Validate(s, "")
	if !hasErrorContaining(errs, "duration") {
		t.Errorf("an unreadable wait duration passed validation; errors: %v", errs)
	}
}
