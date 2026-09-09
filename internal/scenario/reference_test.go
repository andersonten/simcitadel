package scenario

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// referenceExampleDir is the reference environment that SDD section 14.4 runs
// its acceptance test against.
const referenceExampleDir = "examples/reference"

// referenceEnv supplies an address for every variable the reference config
// reads. Constraint C3 of SDD section 14.3 keeps every address out of the
// source, so a test must name them the same way an operator does.
var referenceEnv = map[string]string{
	"WIREMOCK_URL":            "http://127.0.0.1:9090",
	"WIREMOCK_ADMIN_URL":      "http://127.0.0.1:9090/__admin",
	"EVENT_TRIGGER_URL":       "http://127.0.0.1:8081",
	"EVENT_CAPTURE_URL":       "http://127.0.0.1:8082",
	"RABBITMQ_AMQP_URL":       "amqp://guest:guest@127.0.0.1:5672/",
	"RABBITMQ_MANAGEMENT_URL": "http://127.0.0.1:15672",
	"APP_URL":                 "http://127.0.0.1:8080",
	"APP_DB_DSN":              "postgres://app:app@127.0.0.1:5432/app?sslmode=disable",
	"CITADEL_CLIENT_SECRET":   "placeholder",
	"RABBITMQ_USER":           "guest",
	"RABBITMQ_PASSWORD":       "guest",
	"UPSTREAM_SHADOW_DSN":     "postgres://app:app@127.0.0.1:5433/upstream?sslmode=disable",
	"DEBEZIUM_URL":            "http://127.0.0.1:8083",
	"CITADEL_CLIENT_ID":       "citadel",
	"CITADEL_TOKEN_URL":       "http://127.0.0.1:8084/token",
}

// repoRoot returns the module root, found by walking up from the package
// directory to the directory that holds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find go.mod above the package directory")
		}
		dir = parent
	}
}

// TestReferenceScenarios_PassEveryValidationCheck holds the shipped reference
// environment to every check of SDD section 13. The acceptance test of section
// 14.4 and constraint C2 of section 14.3 both run this scenario, so a reference
// scenario that does not validate blocks both. A unit test says so in a second
// rather than at the end of a container run.
func TestReferenceScenarios_PassEveryValidationCheck(t *testing.T) {
	root := repoRoot(t)
	exampleDir := filepath.Join(root, referenceExampleDir)

	for name, value := range referenceEnv {
		t.Setenv(name, value)
	}

	cfg, err := config.Load(filepath.Join(exampleDir, "citadel.yaml"))
	if err != nil {
		t.Fatalf("load the reference config: %v", err)
	}

	paths, err := ExpandPaths([]string{filepath.Join(exampleDir, "scenarios")})
	if err != nil {
		t.Fatalf("expand the reference scenario directory: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("the reference environment ships no scenario")
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, err := Parse(path)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}

			errs := ValidateWithOptions(s, Options{BasePath: exampleDir, Config: cfg})
			for _, e := range errs {
				t.Errorf("check of SDD section 13 failed: %v", e)
			}
		})
	}
}

// TestReferenceConfig_HoldsNoLiteralSecret holds check 12 of SDD section 13 on
// the shipped config.
func TestReferenceConfig_HoldsNoLiteralSecret(t *testing.T) {
	root := repoRoot(t)

	for name, value := range referenceEnv {
		t.Setenv(name, value)
	}

	cfg, err := config.Load(filepath.Join(root, referenceExampleDir, "citadel.yaml"))
	if err != nil {
		t.Fatalf("load the reference config: %v", err)
	}

	for _, e := range ValidateConfigSecrets(cfg) {
		t.Errorf("the reference config holds a literal secret: %v", e)
	}
}
