package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Emitter writes run events to an output stream.
type Emitter struct {
	w      io.Writer
	format string
	runID  string
}

// NewEmitter creates an output emitter.
// The format parameter must be "json" or "human".
func NewEmitter(w io.Writer, format string, runID string) *Emitter {
	return &Emitter{w: w, format: format, runID: runID}
}

// RunStart emits the run start event.
func (e *Emitter) RunStart(scenario, environment string) {
	if e.format == "json" {
		e.writeJSON(map[string]any{
			"ts":            nowRFC3339(),
			"runId":         e.runID,
			"type":          "run.start",
			"schemaVersion": 1,
			"scenario":      scenario,
			"environment":   environment,
		})
		return
	}
	fmt.Fprintf(e.w, "[run.start] scenario=%s environment=%s\n", scenario, environment)
}

// StepStart emits a step start event.
func (e *Emitter) StepStart(index int, name, stepType string) {
	if e.format == "json" {
		e.writeJSON(map[string]any{
			"ts":       nowRFC3339(),
			"runId":    e.runID,
			"type":     "step.start",
			"index":    index,
			"name":     name,
			"stepType": stepType,
		})
		return
	}
	fmt.Fprintf(e.w, "[step %d] START %s (%s)\n", index, name, stepType)
}

// StepPass emits a step pass event.
func (e *Emitter) StepPass(index int, durationMs int64) {
	if e.format == "json" {
		e.writeJSON(map[string]any{
			"ts":         nowRFC3339(),
			"runId":      e.runID,
			"type":       "step.pass",
			"index":      index,
			"durationMs": durationMs,
		})
		return
	}
	fmt.Fprintf(e.w, "[step %d] PASS (%dms)\n", index, durationMs)
}

// stepFailSchemaKeys names the keys that the failure object of SDD section 4.6
// carries beyond the message. A key that the step cannot supply carries null,
// so a consumer reads one stable schema and never has to tell an absent key
// from an unknown value.
var stepFailSchemaKeys = []string{"jsonPath", "attempts"}

// StepFail emits a step fail event with failure details.
// The failureClass parameter classifies the failure as one of:
// assertion, environment, timeout, input, internal.
func (e *Emitter) StepFail(index int, durationMs int64, failure map[string]any, failureClass string) {
	if e.format == "json" {
		e.writeJSON(map[string]any{
			"ts":         nowRFC3339(),
			"runId":      e.runID,
			"type":       "step.fail",
			"index":      index,
			"durationMs": durationMs,
			"failure":    failureObject(failure, failureClass),
		})
		return
	}
	msg := ""
	if m, ok := failure["message"]; ok {
		msg = fmt.Sprintf(": %v", m)
	}
	fmt.Fprintf(e.w, "[step %d] FAIL (%dms)%s\n", index, durationMs, msg)
}

// failureObject renders the failure object of the step.fail event. It copies
// the map the caller holds, because the caller keeps its own copy for the
// steps.jsonl record of SDD section 12.1.
func failureObject(failure map[string]any, failureClass string) map[string]any {
	object := make(map[string]any, len(failure)+len(stepFailSchemaKeys)+1)
	for key, value := range failure {
		object[key] = value
	}
	if failureClass != "" {
		object["class"] = failureClass
	}
	for _, key := range stepFailSchemaKeys {
		if _, ok := object[key]; !ok {
			object[key] = nil
		}
	}
	return object
}

// CheckFail emits the failure of one end-of-run check of SDD section 4.11.
// A consumer of the JSON stream reads run.end with a non-zero exit code, so
// the reason for the failure must reach the same stream.
func (e *Emitter) CheckFail(check, failureClass, message string) {
	if e.format == "json" {
		e.writeJSON(map[string]any{
			"ts":    nowRFC3339(),
			"runId": e.runID,
			"type":  "check.fail",
			"check": check,
			"failure": map[string]any{
				"class":   failureClass,
				"message": message,
			},
		})
		return
	}
	fmt.Fprintf(e.w, "[check.fail] %s: %s\n", check, message)
}

// RunEnd emits the run end event.
func (e *Emitter) RunEnd(outcome string, exitCode int, bundlePath string) {
	if e.format == "json" {
		evt := map[string]any{
			"ts":       nowRFC3339(),
			"runId":    e.runID,
			"type":     "run.end",
			"outcome":  outcome,
			"exitCode": exitCode,
		}
		if bundlePath != "" {
			evt["bundle"] = bundlePath
		}
		e.writeJSON(evt)
		return
	}
	fmt.Fprintf(e.w, "[run.end] %s exit=%d\n", outcome, exitCode)
}

func (e *Emitter) writeJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = e.w.Write(data)
}

// nowRFC3339 formats the current instant to millisecond precision, so two
// events inside the same second keep their order in the stream.
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
