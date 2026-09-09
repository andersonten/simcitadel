package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

func validScenario() *Scenario {
	return &Scenario{
		Name: "valid-scenario",
		Variables: map[string]any{
			"articleId": "12345",
		},
		Steps: []Step{
			{
				Name:         "Set state",
				Type:         "wiremock/set-state",
				ScenarioName: "editorial-flow",
				State:        "pre-submission",
			},
			{
				Name:       "Publish event",
				Type:       "event/publish",
				Exchange:   "public.orchestration-engine.Article.v1",
				RoutingKey: "article-submitted",
				Data:       map[string]any{"key": "value"},
			},
			{
				Name: "Wait for task",
				Type: "await",
				GraphQL: &GraphQLSource{
					Query:     "{ tasks { id } }",
					Variables: map[string]any{"subjectId": "{{ .articleId }}"},
				},
				Expect: &Expect{
					JSONPath: "$.data.tasks[0].status",
					Equals:   "OPEN",
				},
			},
		},
	}
}

func TestValidate_ValidScenario_NoErrors(t *testing.T) {
	s := validScenario()
	errs := Validate(s, "")
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got %d: %v", len(errs), errs)
	}
}

func TestValidate_UnknownStepType_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{Name: "bad step", Type: "unknown/type"},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for an unknown step type")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "unknown step type") && strings.Contains(e.Error(), "unknown/type") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the unknown step type: %v", errs)
	}
}

func TestValidate_PublishMissingExchange_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:       "publish without exchange",
			Type:       "event/publish",
			RoutingKey: "some-key",
			Data:       "payload",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing exchange")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires exchange") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing exchange: %v", errs)
	}
}

func TestValidate_SetStateMissingScenarioName_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:  "set state without scenario",
			Type:  "wiremock/set-state",
			State: "active",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing scenarioName")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires scenarioName") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing scenarioName: %v", errs)
	}
}

func TestValidate_WaitMissingReason_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:     "wait without reason",
			Type:     "wait",
			Duration: "5s",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing reason")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires reason") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing reason: %v", errs)
	}
}

func TestValidate_NotCapturedNoSelector_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name: "not captured with no selector",
			Type: "event/not-captured",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a not-captured step with no selector")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires at least one selector") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing selector: %v", errs)
	}
}

func TestValidate_DuplicateAsNames_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:       "first",
			Type:       "event/publish",
			As:         "msg",
			Exchange:   "ex",
			RoutingKey: "rk",
			Data:       "x",
		},
		{
			Name:       "second",
			Type:       "event/publish",
			As:         "msg",
			Exchange:   "ex",
			RoutingKey: "rk",
			Data:       "y",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for duplicate as names")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "duplicate as name") && strings.Contains(e.Error(), "msg") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the duplicate as name: %v", errs)
	}
}

func TestValidate_SqlExecMissingTarget_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:  "exec without target",
			Type:  "sql/exec",
			Query: "DELETE FROM foo",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing target")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires target") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing target: %v", errs)
	}
}

func TestValidate_ProbeMissingSource_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:   "probe without source",
			Type:   "probe",
			Expect: &Expect{Status: 200},
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a probe without a source")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires at least one source") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing source: %v", errs)
	}
}

func TestValidate_WaitMissingDuration_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:   "wait without duration",
			Type:   "wait",
			Reason: "settle time",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing duration")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires duration") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing duration: %v", errs)
	}
}

func TestValidate_EventCapturedMissingExpect_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:     "captured without expect",
			Type:     "event/captured",
			Exchange: "public.ex",
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing expect")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires expect") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing expect: %v", errs)
	}
}

func TestValidate_InboxAssertMissingWhere_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:   "inbox without where",
			Type:   "inbox/assert",
			Target: "app-db",
			Expect: &Expect{Rows: intPtr(1)},
		},
	}
	errs := Validate(s, "")
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing where")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "requires where") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing where: %v", errs)
	}
}

func TestValidate_FileRefMissing_ReturnsError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:       "publish with missing file",
			Type:       "event/publish",
			Exchange:   "ex",
			RoutingKey: "rk",
			DataFile:   "nonexistent/payload.json",
		},
	}
	errs := Validate(s, t.TempDir())
	if len(errs) == 0 {
		t.Fatal("expected an error for a missing file reference")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "does not exist") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error mentions the missing file: %v", errs)
	}
}

func TestValidate_NotCapturedWithExchange_NoError(t *testing.T) {
	s := validScenario()
	s.Steps = []Step{
		{
			Name:     "not captured with exchange",
			Type:     "event/not-captured",
			Exchange: "public.orchestration-engine.Article.v1",
		},
	}
	errs := Validate(s, "")
	for _, e := range errs {
		if strings.Contains(e.Error(), "selector") {
			t.Fatalf("unexpected selector error: %v", e)
		}
	}
}

func TestValidate_MultipleErrors_ReturnsAll(t *testing.T) {
	s := &Scenario{
		Name: "multi-error",
		Steps: []Step{
			{Name: "bad type", Type: "fake/type"},
			{Name: "wait no reason", Type: "wait", Duration: "1s"},
		},
	}
	errs := Validate(s, "")
	if len(errs) < 2 {
		t.Fatalf("expected at least 2 errors, got %d: %v", len(errs), errs)
	}
}

func TestValidate_StepMissingName_ReturnsError(t *testing.T) {
	s := &Scenario{
		Name: "test-missing-name",
		Steps: []Step{
			{Type: "wait", Duration: "1s", Reason: "settle"},
		},
	}
	errs := Validate(s, "")
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "step name is required") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a step-name-required error, got: %v", errs)
	}
}

func TestValidate_WireMockInAssertions_ReturnsError(t *testing.T) {
	s := &Scenario{
		Name: "wiremock-assertion",
		Steps: []Step{
			{
				Name:       "publish",
				Type:       "event/publish",
				Exchange:   "ex",
				RoutingKey: "rk",
				Data:       "x",
			},
		},
		Assertions: []Step{
			{
				Name:         "bad assertion",
				Type:         "wiremock/set-state",
				ScenarioName: "flow",
				State:        "active",
			},
		},
	}
	errs := Validate(s, "")
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "assertions must not target wiremock") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a wiremock-assertion error, got: %v", errs)
	}
}

func TestValidate_WireMockTargetInAssertions_ReturnsError(t *testing.T) {
	s := &Scenario{
		Name: "wiremock-target-assertion",
		Steps: []Step{
			{
				Name:       "publish",
				Type:       "event/publish",
				Exchange:   "ex",
				RoutingKey: "rk",
				Data:       "x",
			},
		},
		Assertions: []Step{
			{
				Name:   "bad target",
				Type:   "sql/query",
				Target: "wiremock",
				Query:  "SELECT 1",
				Expect: &Expect{Rows: intPtr(1)},
			},
		},
	}
	errs := Validate(s, "")
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "assertions must not target wiremock") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a wiremock-assertion error, got: %v", errs)
	}
}

func TestValidate_InvalidJSONPayload_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "payloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payloads", "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "bad-json",
		Steps: []Step{
			{
				Name:       "publish bad payload",
				Type:       "event/publish",
				Exchange:   "ex",
				RoutingKey: "rk",
				DataFile:   "payloads/bad.json",
			},
		},
	}
	errs := Validate(s, dir)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "invalid JSON") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected an invalid-JSON error, got: %v", errs)
	}
}

func TestValidate_ValidJSONPayload_NoJSONError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "payloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "payloads", "good.json"), []byte(`{"key":"value"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "good-json",
		Steps: []Step{
			{
				Name:       "publish good payload",
				Type:       "event/publish",
				Exchange:   "ex",
				RoutingKey: "rk",
				DataFile:   "payloads/good.json",
			},
		},
	}
	errs := Validate(s, dir)
	for _, e := range errs {
		if strings.Contains(e.Error(), "invalid JSON") {
			t.Fatalf("unexpected JSON error: %v", e)
		}
	}
}

func TestValidate_BadGraphQLQueryFile_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "queries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queries", "bad.graphql"), []byte("SELECT * FROM foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "bad-graphql",
		Steps: []Step{
			{
				Name: "probe with bad query",
				Type: "probe",
				GraphQL: &GraphQLSource{
					QueryFile: "queries/bad.graphql",
				},
				Expect: &Expect{Status: 200},
			},
		},
	}
	errs := Validate(s, dir)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "valid GraphQL operation") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a GraphQL syntax error, got: %v", errs)
	}
}

func TestValidate_ValidGraphQLQueryFile_NoGraphQLError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "queries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queries", "good.graphql"), []byte("query { tasks { id } }"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "good-graphql",
		Steps: []Step{
			{
				Name: "probe with good query",
				Type: "probe",
				GraphQL: &GraphQLSource{
					QueryFile: "queries/good.graphql",
				},
				Expect: &Expect{Status: 200},
			},
		},
	}
	errs := Validate(s, dir)
	for _, e := range errs {
		if strings.Contains(e.Error(), "valid GraphQL operation") {
			t.Fatalf("unexpected GraphQL error: %v", e)
		}
	}
}

func TestValidate_WireMockStubMissingNewScenarioState_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	stub := `{
		"scenarioName": "flow",
		"postServeActions": [{"name": "webhook"}]
	}`
	if err := os.WriteFile(filepath.Join(dir, "stub.json"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "stub-check",
		Steps: []Step{
			{
				Name:       "publish",
				Type:       "event/publish",
				Exchange:   "ex",
				RoutingKey: "rk",
				Data:       "x",
			},
		},
	}
	errs := Validate(s, dir)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "newScenarioState") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a newScenarioState error, got: %v", errs)
	}
}

func TestValidate_WireMockStubWithNewScenarioState_NoError(t *testing.T) {
	dir := t.TempDir()
	stub := `{
		"scenarioName": "flow",
		"newScenarioState": "next",
		"postServeActions": [{"name": "webhook"}]
	}`
	if err := os.WriteFile(filepath.Join(dir, "stub.json"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "stub-check-ok",
		Steps: []Step{
			{
				Name:       "publish",
				Type:       "event/publish",
				Exchange:   "ex",
				RoutingKey: "rk",
				Data:       "x",
			},
		},
	}
	errs := Validate(s, dir)
	for _, e := range errs {
		if strings.Contains(e.Error(), "newScenarioState") {
			t.Fatalf("unexpected newScenarioState error: %v", e)
		}
	}
}

func TestValidate_SetStateMismatchScenarioName_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	stub := `{"scenarioName": "editorial-flow"}`
	if err := os.WriteFile(filepath.Join(dir, "stub.json"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "set-state-mismatch",
		Steps: []Step{
			{
				Name:         "set state",
				Type:         "wiremock/set-state",
				ScenarioName: "nonexistent-flow",
				State:        "active",
			},
		},
	}
	errs := Validate(s, dir)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "does not match any stub file") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a scenario-name mismatch error, got: %v", errs)
	}
}

func TestValidate_SetStateMatchingScenarioName_NoError(t *testing.T) {
	dir := t.TempDir()
	stub := `{"scenarioName": "editorial-flow"}`
	if err := os.WriteFile(filepath.Join(dir, "stub.json"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scenario{
		Name: "set-state-match",
		Steps: []Step{
			{
				Name:         "set state",
				Type:         "wiremock/set-state",
				ScenarioName: "editorial-flow",
				State:        "active",
			},
		},
	}
	errs := Validate(s, dir)
	for _, e := range errs {
		if strings.Contains(e.Error(), "does not match any stub file") {
			t.Fatalf("unexpected mismatch error: %v", e)
		}
	}
}

func TestValidateConfigSecrets_LiteralClientSecret_ReturnsError(t *testing.T) {
	cfg := &config.CitadelConfig{
		Auth: map[string]config.AuthConfig{
			"default": {
				TokenURL:     "https://auth.example.com/token",
				ClientID:     "my-client",
				ClientSecret: "super-secret-value",
			},
		},
		Targets: config.TargetsConfig{},
	}
	errs := ValidateConfigSecrets(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "literal value") && strings.Contains(e.Error(), "clientSecret") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a literal-secret error for clientSecret, got: %v", errs)
	}
}

func TestValidateConfigSecrets_InterpolatedSecret_NoError(t *testing.T) {
	cfg := &config.CitadelConfig{
		Auth: map[string]config.AuthConfig{
			"default": {
				TokenURL:     "https://auth.example.com/token",
				ClientID:     "my-client",
				ClientSecret: "${AUTH_SECRET}",
			},
		},
		Targets: config.TargetsConfig{},
	}
	errs := ValidateConfigSecrets(cfg)
	for _, e := range errs {
		if strings.Contains(e.Error(), "clientSecret") {
			t.Fatalf("unexpected secret error: %v", e)
		}
	}
}

func TestValidateConfigSecrets_LiteralDSN_ReturnsError(t *testing.T) {
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			AppDB: &config.DatabaseTarget{
				Driver: "postgres",
				DSN:    "postgres://user:pass@host:5432/db",
			},
		},
	}
	errs := ValidateConfigSecrets(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "literal value") && strings.Contains(e.Error(), "dsn") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a literal-secret error for dsn, got: %v", errs)
	}
}

func intPtr(n int) *int {
	return &n
}
