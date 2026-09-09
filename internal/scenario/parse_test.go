package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validScenarioYAML = `name: editorial-assignment-happy-path
description: Verify that article submission triggers an editor assignment task
variables:
  articleId: "12345"
  spaceId: "space-1"
reset:
  truncate:
    - outbox
    - inbox
steps:
  - name: Set WireMock to pre-submission state
    type: wiremock/set-state
    scenarioName: editorial-flow
    state: pre-submission
  - name: Publish article-submitted event
    type: event/publish
    exchange: public.orchestration-engine.Article.v1
    routingKey: article-submitted
    dataFile: payloads/article-submitted.v1.0.0.json
  - name: Wait for the engine to open the assignment task
    type: await
    as: assignmentTask
    graphql:
      queryFile: queries/pe-tasks.graphql
      variables:
        subjectId: "{{ .articleId }}"
    expect:
      jsonPath: "$.data.pe_tasks.items[0].status"
      equals: "OPEN"
    timeout: 20s
assertions:
  - name: Verify the outbox recorded the event
    type: sql/query
    target: app-db
    query: SELECT count(*) as cnt FROM outbox WHERE aggregateid = '{{ .articleId }}'
    expect:
      columns:
        cnt: 1
after:
  - name: Clean up test data
    type: sql/exec
    target: app-db
    query: DELETE FROM outbox WHERE aggregateid = '{{ .articleId }}'
`

func writeScenario(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write test scenario: %v", err)
	}
	return p
}

func TestParse_ValidScenario_ParsesAllFields(t *testing.T) {
	p := writeScenario(t, validScenarioYAML)
	s, err := Parse(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name != "editorial-assignment-happy-path" {
		t.Fatalf("name: got %q", s.Name)
	}
	if s.Description == "" {
		t.Fatal("description is empty")
	}
	if len(s.Variables) != 2 {
		t.Fatalf("variables: got %d, want 2", len(s.Variables))
	}
	if s.Variables["articleId"] != "12345" {
		t.Fatalf("variables.articleId: got %v", s.Variables["articleId"])
	}
	if s.Reset == nil {
		t.Fatal("reset is nil")
	}
	if len(s.Reset.Truncate) != 2 {
		t.Fatalf("reset.truncate: got %d, want 2", len(s.Reset.Truncate))
	}
	if len(s.Steps) != 3 {
		t.Fatalf("steps: got %d, want 3", len(s.Steps))
	}
	if s.Steps[0].Type != "wiremock/set-state" {
		t.Fatalf("steps[0].type: got %q", s.Steps[0].Type)
	}
	if s.Steps[0].ScenarioName != "editorial-flow" {
		t.Fatalf("steps[0].scenarioName: got %q", s.Steps[0].ScenarioName)
	}
	if s.Steps[1].Type != "event/publish" {
		t.Fatalf("steps[1].type: got %q", s.Steps[1].Type)
	}
	if s.Steps[1].Exchange != "public.orchestration-engine.Article.v1" {
		t.Fatalf("steps[1].exchange: got %q", s.Steps[1].Exchange)
	}
	if s.Steps[2].Type != "await" {
		t.Fatalf("steps[2].type: got %q", s.Steps[2].Type)
	}
	if s.Steps[2].As != "assignmentTask" {
		t.Fatalf("steps[2].as: got %q", s.Steps[2].As)
	}
	if s.Steps[2].GraphQL == nil {
		t.Fatal("steps[2].graphql is nil")
	}
	if s.Steps[2].GraphQL.QueryFile != "queries/pe-tasks.graphql" {
		t.Fatalf("steps[2].graphql.queryFile: got %q", s.Steps[2].GraphQL.QueryFile)
	}
	if s.Steps[2].Expect == nil {
		t.Fatal("steps[2].expect is nil")
	}
	if s.Steps[2].Timeout != "20s" {
		t.Fatalf("steps[2].timeout: got %q", s.Steps[2].Timeout)
	}
	if len(s.Assertions) != 1 {
		t.Fatalf("assertions: got %d, want 1", len(s.Assertions))
	}
	if s.Assertions[0].Type != "sql/query" {
		t.Fatalf("assertions[0].type: got %q", s.Assertions[0].Type)
	}
	if len(s.After) != 1 {
		t.Fatalf("after: got %d, want 1", len(s.After))
	}
	if s.After[0].Type != "sql/exec" {
		t.Fatalf("after[0].type: got %q", s.After[0].Type)
	}
}

func TestParse_MissingName_ReturnsError(t *testing.T) {
	yaml := `steps:
  - name: do something
    type: wait
    duration: 1s
    reason: test
`
	p := writeScenario(t, yaml)
	_, err := Parse(p)
	if err == nil {
		t.Fatal("expected an error for a missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("error %q does not mention missing name", err.Error())
	}
}

func TestParse_EmptySteps_ReturnsError(t *testing.T) {
	yaml := `name: empty-scenario
steps: []
`
	p := writeScenario(t, yaml)
	_, err := Parse(p)
	if err == nil {
		t.Fatal("expected an error for empty steps")
	}
	if !strings.Contains(err.Error(), "no steps") {
		t.Fatalf("error %q does not mention empty steps", err.Error())
	}
}

func TestParse_UnknownKey_ReturnsError(t *testing.T) {
	yaml := `name: bad-scenario
unknownField: true
steps:
  - name: do something
    type: wait
    duration: 1s
    reason: test
`
	p := writeScenario(t, yaml)
	_, err := Parse(p)
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "unknownField") {
		t.Fatalf("error %q does not name the unknown key", err.Error())
	}
}

func TestParseBytes_ValidYAML_ParsesScenario(t *testing.T) {
	s, err := ParseBytes([]byte(validScenarioYAML))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name != "editorial-assignment-happy-path" {
		t.Fatalf("name: got %q", s.Name)
	}
	if len(s.Steps) != 3 {
		t.Fatalf("steps: got %d, want 3", len(s.Steps))
	}
}

func TestParseBytes_RefField_Accepted(t *testing.T) {
	y := `name: ref-scenario
steps:
  - name: step with ref
    type: event/publish
    ref: article-submitted-v1
    exchange: ex
    routingKey: rk
    data: payload
`
	s, err := ParseBytes([]byte(y))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Steps[0].Ref != "article-submitted-v1" {
		t.Fatalf("ref: got %q, want %q", s.Steps[0].Ref, "article-submitted-v1")
	}
}

func TestParseBytes_ResetFields_Accepted(t *testing.T) {
	y := `name: reset-scenario
reset:
  truncate:
    - outbox
steps:
  - name: do something
    type: wait
    duration: 1s
    reason: test
`
	s, err := ParseBytes([]byte(y))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.Reset.Truncate) != 1 || s.Reset.Truncate[0] != "outbox" {
		t.Fatalf("truncate: got %v, want [outbox]", s.Reset.Truncate)
	}
}
