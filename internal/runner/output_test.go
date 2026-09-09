package runner

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitter_JSON_RunStart_CorrectFormat(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf, "json", "test-run-id")

	e.RunStart("my-scenario", "dev")

	var evt map[string]any
	if err := json.Unmarshal(buf.Bytes(), &evt); err != nil {
		t.Fatalf("unmarshal run.start event: %v", err)
	}

	if evt["type"] != "run.start" {
		t.Fatalf("type = %v, want run.start", evt["type"])
	}
	if evt["runId"] != "test-run-id" {
		t.Fatalf("runId = %v, want test-run-id", evt["runId"])
	}
	if evt["scenario"] != "my-scenario" {
		t.Fatalf("scenario = %v, want my-scenario", evt["scenario"])
	}
	if evt["environment"] != "dev" {
		t.Fatalf("environment = %v, want dev", evt["environment"])
	}
	if _, ok := evt["ts"]; !ok {
		t.Fatal("event is missing ts field")
	}
	if evt["schemaVersion"] != float64(1) {
		t.Fatalf("schemaVersion = %v, want 1", evt["schemaVersion"])
	}
}

func TestEmitter_JSON_StepFail_IncludesFailureClass(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf, "json", "run-class")

	e.StepFail(2, 500, map[string]any{"message": "timeout reached"}, "timeout")

	var evt map[string]any
	if err := json.Unmarshal(buf.Bytes(), &evt); err != nil {
		t.Fatalf("unmarshal step.fail event: %v", err)
	}

	if evt["type"] != "step.fail" {
		t.Fatalf("type = %v, want step.fail", evt["type"])
	}
	failure, ok := evt["failure"].(map[string]any)
	if !ok {
		t.Fatal("failure field is not a map")
	}
	if failure["class"] != "timeout" {
		t.Fatalf("failure.class = %v, want timeout", failure["class"])
	}
	if failure["message"] != "timeout reached" {
		t.Fatalf("failure.message = %v, want timeout reached", failure["message"])
	}
}

func TestEmitter_JSON_StepPass_CorrectFormat(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf, "json", "run-123")

	e.StepPass(1, 68)

	var evt map[string]any
	if err := json.Unmarshal(buf.Bytes(), &evt); err != nil {
		t.Fatalf("unmarshal step.pass event: %v", err)
	}

	if evt["type"] != "step.pass" {
		t.Fatalf("type = %v, want step.pass", evt["type"])
	}
	if evt["index"] != float64(1) {
		t.Fatalf("index = %v, want 1", evt["index"])
	}
	if evt["durationMs"] != float64(68) {
		t.Fatalf("durationMs = %v, want 68", evt["durationMs"])
	}
}

func TestEmitter_Human_StepPass_ReadableFormat(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf, "human", "run-456")

	e.StepPass(1, 68)

	line := strings.TrimSpace(buf.String())
	expected := "[step 1] PASS (68ms)"
	if line != expected {
		t.Fatalf("output = %q, want %q", line, expected)
	}
}

func TestEmitter_Human_StepFail_IncludesMessage(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf, "human", "run-789")

	e.StepFail(1, 20001, map[string]any{"message": "expected OPEN, got null"}, "assertion")

	line := strings.TrimSpace(buf.String())
	expected := "[step 1] FAIL (20001ms): expected OPEN, got null"
	if line != expected {
		t.Fatalf("output = %q, want %q", line, expected)
	}
}

func TestEmitter_Human_RunEnd_IncludesExitCode(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf, "human", "run-end")

	e.RunEnd("FAIL", 1, "")

	line := strings.TrimSpace(buf.String())
	expected := "[run.end] FAIL exit=1"
	if line != expected {
		t.Fatalf("output = %q, want %q", line, expected)
	}
}
