package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The list of outbound exchanges lives in two places. The reference config names
// it under capture.exchanges, and the compose file carries the same list as the
// default of EVENT_CAPTURE_EXCHANGES, because nothing else in a Citadel
// environment declares those exchanges under SDD section 7.1.
//
// The two agreed when the compose default was written, and nothing held them
// together. An empty default was the shipped state until this round, and it made
// preflight fail the first run at exit 3, which is the class of first-run
// failure that SDD section 11.1 lists its rules to remove. A list that drifts
// costs the same failure, so the drift is a test rather than a comment.

// captureExchangesDefault reads the default of EVENT_CAPTURE_EXCHANGES out of
// the compose file. It reads the text rather than run docker compose, so the
// check needs no daemon.
var captureExchangesDefault = regexp.MustCompile(
	`EVENT_CAPTURE_EXCHANGES:\s*"\$\{CAPTURE_EXCHANGES:-([^}"]*)\}"`)

// loadReferenceConfig loads the shipped example config. Constraint C3 of SDD section 1
// keeps every address out of the binary, so the config names an environment
// variable for each one and the test must supply them.
func loadReferenceConfig(t *testing.T, root string) *CitadelConfig {
	t.Helper()

	for name, value := range map[string]string{
		"WIREMOCK_URL":            "http://wiremock:8080",
		"WIREMOCK_ADMIN_URL":      "http://wiremock:8080/__admin",
		"RABBITMQ_AMQP_URL":       "amqp://guest:guest@rabbitmq:5672/",
		"RABBITMQ_MANAGEMENT_URL": "http://rabbitmq:15672",
		"APP_URL":                 "http://app:8080",
		"APP_DB_DSN":              "postgres://citadel:citadel@app-db:5432/citadel?sslmode=disable",
		"EVENT_TRIGGER_URL":       "http://event-trigger:8080",
		"EVENT_CAPTURE_URL":       "http://event-capture:8080",
	} {
		t.Setenv(name, value)
	}

	cfg, err := Load(filepath.Join(root, "examples", "reference", "citadel.yaml"))
	if err != nil {
		t.Fatalf("load the reference config: %v", err)
	}
	return cfg
}

func TestComposeDefault_CaptureExchanges_MatchTheReferenceConfig(t *testing.T) {
	root := moduleRoot(t)

	cfg := loadReferenceConfig(t, root)
	if len(cfg.Capture.Exchanges) == 0 {
		t.Fatal("the reference config names no capture exchange, so this test proves nothing")
	}

	composePath := filepath.Join(root, "deploy", "compose", "docker-compose.yml")
	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read the compose file: %v", err)
	}

	match := captureExchangesDefault.FindSubmatch(compose)
	if match == nil {
		t.Fatal("the compose file carries no default for EVENT_CAPTURE_EXCHANGES, so the capture service binds nothing on a first run")
	}

	// The variable takes a comma-separated list, and each entry is a name or a
	// name and a declare mode after a colon.
	got := map[string]string{}
	for _, entry := range strings.Split(string(match[1]), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, mode := entry, "own"
		if i := strings.LastIndex(entry, ":"); i >= 0 {
			name, mode = entry[:i], entry[i+1:]
		}
		got[name] = mode
	}

	want := map[string]string{}
	for _, ex := range cfg.Capture.Exchanges {
		mode := ex.Declare
		if mode == "" {
			mode = "own"
		}
		want[ex.Name] = mode
	}

	for name, mode := range want {
		gotMode, ok := got[name]
		if !ok {
			t.Errorf("the compose default does not bind %s, which capture.exchanges of the reference config names", name)
			continue
		}
		if gotMode != mode {
			t.Errorf("the compose default declares %s as %q, and the reference config declares it as %q", name, gotMode, mode)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("the compose default binds %s, which the reference config does not name", name)
		}
	}
}

// TestComposeDefault_SpyQueues_ExistForEveryCaptureExchange holds the third copy
// of the same list. SDD section 7.3 declares a spy queue per exchange in
// definitions.json, and the capture service binds a queue that must be there.
func TestComposeDefault_SpyQueues_ExistForEveryCaptureExchange(t *testing.T) {
	root := moduleRoot(t)

	cfg := loadReferenceConfig(t, root)

	definitions, err := os.ReadFile(filepath.Join(root, "deploy", "compose", "definitions.json"))
	if err != nil {
		t.Fatalf("read the broker definitions: %v", err)
	}

	for _, ex := range cfg.Capture.Exchanges {
		queue := "citadel.spy." + ex.Name
		if !strings.Contains(string(definitions), queue) {
			t.Errorf("definitions.json declares no queue %s for the exchange %s of capture.exchanges", queue, ex.Name)
		}
	}
}

// moduleRoot walks up from the test directory to the directory that holds go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
