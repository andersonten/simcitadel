package expect

import (
	"testing"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// conformingEnvelope returns a CloudEvents 1.0 envelope that also carries the
// required version attribute.
func conformingEnvelope() map[string]any {
	return map[string]any{
		"specversion": "1.0",
		"type":        "Manuscript.Created",
		"source":      "ORCHESTRATION-ENGINE",
		"id":          "11111111-1111-1111-1111-111111111111",
		"version":     "v1",
		"data":        map[string]any{"manuscriptId": "515098"},
	}
}

// captureEntry wraps an envelope in the capture service entry of SDD 8.2.
func captureEntry(body map[string]any) map[string]any {
	return map[string]any{
		"seq":        412,
		"receivedAt": "2026-08-19T09:00:12.500Z",
		"exchange":   "public.orchestration-engine.Manuscript.v1",
		"routingKey": "public.orchestration-engine.Manuscript.v1",
		"headers":    map[string]any{"eventType": "Manuscript.Created"},
		"runId":      "0191",
		"body":       body,
	}
}

// The capture service wraps each envelope under "body". The check must look at
// the envelope, not at the wrapper.
func TestEvaluate_CloudEventsValid_CaptureEntry_PassesOnValidEnvelope(t *testing.T) {
	res := Evaluate(&scenario.Expect{CloudEventsValid: true}, []any{captureEntry(conformingEnvelope())})
	if !res.Pass {
		t.Errorf("expected pass, got %v: %s", res.Pass, res.Message)
	}
}

func TestEvaluate_CloudEventsValid_BareEnvelope_PassesOnValidEnvelope(t *testing.T) {
	res := Evaluate(&scenario.Expect{CloudEventsValid: true}, []any{conformingEnvelope()})
	if !res.Pass {
		t.Errorf("expected pass, got %v: %s", res.Pass, res.Message)
	}
}

// SDD 4.11 requires CloudEvents 1.0 plus the required version attribute.
func TestValidateCloudEvent_MissingAttribute_ReturnsError(t *testing.T) {
	cases := []string{"specversion", "type", "source", "id", "version"}
	for _, missing := range cases {
		t.Run(missing, func(t *testing.T) {
			env := conformingEnvelope()
			delete(env, missing)
			if err := ValidateCloudEvent(env); err == nil {
				t.Errorf("expected an error when %q is absent", missing)
			}
		})
	}
}

func TestValidateCloudEvent_WrongSpecVersion_ReturnsError(t *testing.T) {
	env := conformingEnvelope()
	env["specversion"] = "0.3"
	if err := ValidateCloudEvent(env); err == nil {
		t.Error("expected an error for specversion 0.3")
	}
}

func TestEvaluate_CloudEventsValid_MissingVersion_Fails(t *testing.T) {
	env := conformingEnvelope()
	delete(env, "version")
	res := Evaluate(&scenario.Expect{CloudEventsValid: true}, []any{captureEntry(env)})
	if res.Pass {
		t.Error("expected a failure for an envelope with no required version attribute")
	}
}
