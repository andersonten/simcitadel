package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// This file locks down check 3 of SDD section 13 against the content
// directories of section 5.1. It began as a proof that validate looked beside
// the scenario file while the runner looked under paths.payloads, so a scenario
// could pass validation and then fail at run time on the same file.

// projectLayout writes a config and a payload in the documented layout and
// returns the loaded config with the scenario directory.
func projectLayout(t *testing.T) (*config.CitadelConfig, string) {
	t.Helper()
	root := t.TempDir()

	for _, dir := range []string{"scenarios", "payloads", "sql", "queries"} {
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
`
	configPath := filepath.Join(root, "citadel.yaml")
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	payload := filepath.Join(root, "payloads", "article-submitted.v1.0.0.json")
	if err := os.WriteFile(payload, []byte(`{"articleId":"a-1"}`), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg, filepath.Join(root, "scenarios")
}

func publishScenario(dataFile string) *Scenario {
	return &Scenario{
		Name: "publish-a-payload",
		Steps: []Step{{
			Name:       "publish the article",
			Type:       "event/publish",
			Exchange:   "public.orchestration-engine.Article.v1",
			RoutingKey: "public.orchestration-engine.Article.v1",
			DataFile:   dataFile,
		}},
	}
}

// TestValidateWithOptions_PayloadUnderPathsPayloads_IsFound holds check 3
// against the layout the runner reads from.
func TestValidateWithOptions_PayloadUnderPathsPayloads_IsFound(t *testing.T) {
	cfg, scenarioDir := projectLayout(t)

	errs := ValidateWithOptions(publishScenario("article-submitted.v1.0.0.json"),
		Options{BasePath: scenarioDir, Config: cfg})

	for _, err := range errs {
		if strings.Contains(err.Error(), "dataFile") {
			t.Errorf("validate did not find the payload the runner reads: %v", err)
		}
	}
}

// TestValidateWithOptions_MissingPayload_NamesThePathItTried keeps the error
// actionable. A developer needs to know where the tool looked.
func TestValidateWithOptions_MissingPayload_NamesThePathItTried(t *testing.T) {
	cfg, scenarioDir := projectLayout(t)

	errs := ValidateWithOptions(publishScenario("not-there.json"),
		Options{BasePath: scenarioDir, Config: cfg})

	var found bool
	for _, err := range errs {
		if strings.Contains(err.Error(), "dataFile") {
			found = true
			if !strings.Contains(err.Error(), filepath.Join("payloads", "not-there.json")) {
				t.Errorf("error %q does not name the path that was tried", err.Error())
			}
		}
	}
	if !found {
		t.Error("a missing payload produced no error")
	}
}

// TestValidateWithOptions_PayloadBesideTheScenario_IsRejected states the
// consequence of the shared resolver: a file in the scenario directory is not
// on the payload path, and the runner would not find it either.
func TestValidateWithOptions_PayloadBesideTheScenario_IsRejected(t *testing.T) {
	cfg, scenarioDir := projectLayout(t)

	beside := filepath.Join(scenarioDir, "beside.json")
	if err := os.WriteFile(beside, []byte(`{"articleId":"a-1"}`), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	errs := ValidateWithOptions(publishScenario("beside.json"),
		Options{BasePath: scenarioDir, Config: cfg})

	var found bool
	for _, err := range errs {
		if strings.Contains(err.Error(), "dataFile") {
			found = true
		}
	}
	if !found {
		t.Error("a payload outside paths.payloads passed validation but would fail at run time")
	}
}

// TestValidate_WithoutConfig_FallsBackToTheScenarioDirectory keeps citadel
// validate usable with no config present, as SDD section 4.1 allows.
func TestValidate_WithoutConfig_FallsBackToTheScenarioDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "beside.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	errs := Validate(publishScenario("beside.json"), dir)
	for _, err := range errs {
		if strings.Contains(err.Error(), "dataFile") {
			t.Errorf("the fallback did not find a file beside the scenario: %v", err)
		}
	}
}
