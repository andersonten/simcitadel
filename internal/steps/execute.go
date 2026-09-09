package steps

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/andersonten/simcitadel/internal/expect"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// StepResult holds the outcome of executing one step.
type StepResult struct {
	Pass     bool
	Duration time.Duration
	Expect   *expect.Result
	Binding  any
	Err      error

	// Attempts counts the probes the step ran. SDD section 4.6 writes the count
	// into the step.fail event and section 12.1 lists it in each steps.jsonl
	// record. A step that runs once reports 1, so a reader never reads 0 and
	// wonders whether the field means "once" or "not recorded".
	Attempts int
}

// Execute runs a single step and returns the result.
// Execute dispatches to the appropriate executor based on step.Type.
func Execute(ctx context.Context, step *scenario.Step, rc *RunContext) (*StepResult, error) {
	if !IsValidType(step.Type) {
		return nil, fmt.Errorf("execute step %q: unknown type %q", step.Name, step.Type)
	}

	if rc.StepThrough {
		fmt.Fprintf(os.Stderr, "[step-through] Press Enter to run step %q (%s)...", step.Name, step.Type)
		buf := make([]byte, 1)
		_, _ = os.Stdin.Read(buf)
	}

	start := time.Now()

	// A step is rendered here rather than when the scenario loads, because a
	// message reference of SDD section 6.4 binds only after the step that
	// declares as: has run.
	rendered, err := scenario.RenderStep(step, rc.TemplateVars())
	if err != nil {
		return nil, fmt.Errorf("execute step %q: %w", step.Name, err)
	}
	step = rendered

	// stepDeadline is the per-step ceiling of SDD section 6.3. The retry loop
	// needs it as a value, because a ceiling that closes an attempt is the
	// scenario's own window and not the target going away.
	var stepDeadline time.Time
	if step.Timeout != "" {
		d, parseErr := time.ParseDuration(step.Timeout)
		if parseErr != nil {
			return nil, fmt.Errorf("execute step %q parse timeout: %w", step.Name, parseErr)
		}
		stepDeadline = time.Now().Add(d)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, stepDeadline)
		defer cancel()
	}

	result, err := dispatch(ctx, step, rc, stepDeadline)
	if err != nil {
		return nil, err
	}
	result.Duration = time.Since(start)
	// Only the retry loop counts its own probes. Every other step runs once, and
	// it reports 1 here rather than 0, because a reader cannot tell 0 from a
	// field that nothing filled.
	if result.Attempts == 0 {
		result.Attempts = 1
	}

	if step.As != "" && result.Pass {
		rc.Bind(step.As, result.Binding)
	}

	return result, nil
}

// dispatch runs one step. A step that could not execute returns an error, so
// the runner can tell an unreachable target from an expectation the target did
// not meet. SDD section 4.5 gives the two different exit codes.
func dispatch(ctx context.Context, step *scenario.Step, rc *RunContext, stepDeadline time.Time) (*StepResult, error) {
	switch step.Type {
	case "event/publish":
		ref, err := ExecutePublish(ctx, step, rc)
		if err != nil {
			return nil, err
		}
		return &StepResult{Pass: true, Binding: ref}, nil

	case "event/republish":
		return executeRepublish(ctx, step, rc)

	case "event/not-captured":
		return executeNotCaptured(ctx, step, rc)

	case "inbox/assert":
		return executeRowStep(ctx, step, rc, stepDeadline, probeInboxRows)

	case "wiremock/set-state":
		err := ExecuteSetState(ctx, step, rc)
		if err != nil {
			return nil, err
		}
		return &StepResult{Pass: true}, nil

	case "sql/exec":
		err := ExecuteSQLExec(ctx, step, rc)
		if err != nil {
			return nil, err
		}
		return &StepResult{Pass: true}, nil

	case "sql/query":
		return executeRowStep(ctx, step, rc, stepDeadline, probeSQL)

	case "probe", "await", "app/graphql", "app/http", "event/captured":
		if step.Type == "event/captured" {
			step.Captured = capturedQueryFromStep(step)
		}
		expResult, probeResult, attempts, err := executeWithRetryProbe(ctx, step, rc, rc.Config.Defaults, stepDeadline, ExecuteProbe)
		if err != nil {
			return nil, err
		}
		sr := &StepResult{
			Pass:     expResult.Pass,
			Expect:   expResult,
			Attempts: attempts,
		}
		if expResult.Pass {
			sr.Binding = probeBinding(step, expResult, probeResult)
		}
		return sr, nil

	case "wait":
		err := ExecuteWait(ctx, step)
		if err != nil {
			return nil, err
		}
		return &StepResult{Pass: true}, nil

	default:
		return nil, fmt.Errorf("dispatch: unhandled step type %q", step.Type)
	}
}

// probeBinding returns the value the as: field of a probe binds. A receiving
// step binds the matched message, as SDD section 6.4 says. Every other probe
// binds what its expectation read, which is the only value it has.
func probeBinding(step *scenario.Step, expResult *expect.Result, probeResult *ProbeResult) any {
	if step.Type == "event/captured" {
		if ref := capturedBinding(probeResult); ref != nil {
			return ref
		}
		return nil
	}
	if expResult.Actual != nil {
		return expResult.Actual
	}
	return nil
}

// executeRowStep runs a step that reads a row set and evaluates an expectation
// over it. It goes through the retry loop, because SDD section 6.3 makes retry
// the only repetition mechanism and a field of the one step schema of section
// 6.1, so sql/query and inbox/assert carry it as much as a probe does. The whole
// row set reaches the evaluator, because section 8.4 combines rows, columns and
// notNull in one expect and no key may hide another.
func executeRowStep(ctx context.Context, step *scenario.Step, rc *RunContext, stepDeadline time.Time, read probeRead) (*StepResult, error) {
	expResult, probeResult, attempts, err := executeWithRetryProbe(ctx, step, rc, rc.Config.Defaults, stepDeadline, read)
	if err != nil {
		return nil, err
	}
	result := &StepResult{
		Pass:     expResult.Pass,
		Expect:   expResult,
		Attempts: attempts,
	}
	if probeResult != nil {
		result.Binding = probeResult.Response
	}
	return result, nil
}
