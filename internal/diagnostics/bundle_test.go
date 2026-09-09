package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBundle_CreatesDirectoryStructure(t *testing.T) {
	dir := t.TempDir()
	bc := BundleConfig{
		BundleDir: dir,
		RunID:     "run-001",
		Scenario:  "name: test-scenario\n",
		Config:    map[string]any{"host": "localhost"},
		Steps:     []map[string]any{{"step": "one"}},
		WireMock: &WireMockDiag{
			Unmatched:  []string{"req1"},
			NearMisses: []string{"miss1"},
			Journal:    []string{"entry1"},
		},
		RabbitMQ: &RabbitMQDiag{
			Queues:   []string{"q1"},
			Topology: map[string]any{"exchange": "e1"},
		},
		Captured: []map[string]any{{"msg": "hello"}},
		Debezium: map[string]any{"connector": "outbox"},
	}

	if err := WriteBundle(bc); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	runDir := filepath.Join(dir, "run-001")
	paths := []string{
		"run.json",
		"scenario.yaml",
		"steps.jsonl",
		"wiremock/unmatched.json",
		"wiremock/near-misses.json",
		"wiremock/journal.json",
		"rabbitmq/queues.json",
		"rabbitmq/topology.json",
		"captured.jsonl",
		"debezium/connectors.json",
	}
	for _, p := range paths {
		full := filepath.Join(runDir, p)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected file %s to exist: %v", p, err)
		}
	}
}

func TestWriteBundle_RunJSON_IsRedacted(t *testing.T) {
	dir := t.TempDir()
	bc := BundleConfig{
		BundleDir: dir,
		RunID:     "run-002",
		Config: map[string]any{
			"host":     "localhost",
			"password": "secret123",
		},
	}

	if err := WriteBundle(bc); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "run-002", "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}

	if parsed["password"] != "«redacted»" {
		t.Errorf("password: got %v, want «redacted»", parsed["password"])
	}
	if parsed["host"] != "localhost" {
		t.Errorf("host: got %v, want localhost", parsed["host"])
	}
}

func TestWriteBundle_StepsJSONL_OneLinePerStep(t *testing.T) {
	dir := t.TempDir()
	bc := BundleConfig{
		BundleDir: dir,
		RunID:     "run-003",
		Steps: []map[string]any{
			{"action": "publish", "exchange": "events"},
			{"action": "await", "timeout": "5s"},
		},
	}

	if err := WriteBundle(bc); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "run-003", "steps.jsonl"))
	if err != nil {
		t.Fatalf("read steps.jsonl: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("line %d: invalid JSON: %v", i, err)
		}
	}
}

func TestWriteBundle_MissingOptionalData_SkipsFiles(t *testing.T) {
	dir := t.TempDir()
	bc := BundleConfig{
		BundleDir: dir,
		RunID:     "run-004",
		Config:    map[string]any{"host": "localhost"},
	}

	if err := WriteBundle(bc); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	runDir := filepath.Join(dir, "run-004")

	if _, err := os.Stat(filepath.Join(runDir, "run.json")); err != nil {
		t.Error("run.json must exist")
	}

	absent := []string{
		"steps.jsonl",
		"wiremock",
		"rabbitmq",
		"captured.jsonl",
		"debezium",
	}
	for _, p := range absent {
		full := filepath.Join(runDir, p)
		if _, err := os.Stat(full); err == nil {
			t.Errorf("expected %s to not exist when data is nil", p)
		}
	}
}
