package steps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/expect"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// ExecuteWithRetry executes a probe with retry according to the step retry configuration.
// If no retry is configured, it runs the probe once.
// For await steps, retry defaults to until pass with the defaults timeout and interval.
func ExecuteWithRetry(ctx context.Context, step *scenario.Step, rc *RunContext, defaults config.DefaultsConfig) (*expect.Result, error) {
	result, _, _, err := executeWithRetryProbe(ctx, step, rc, defaults, time.Time{}, ExecuteProbe)
	return result, err
}

// probeRead reads one probe. SDD section 6.5 selects most reads by source, and
// sql/query and inbox/assert build a read of their own, so the loop takes the
// read as a parameter. Section 6.3 makes retry a field of the one step schema
// rather than a field of the probe types, so every step that evaluates an
// expectation repeats through this loop.
type probeRead func(context.Context, *scenario.Step, *RunContext) (*ProbeResult, error)

// executeWithRetryProbe returns the probe outcome and the attempt count as well
// as the expectation result. The binding of SDD section 6.4 for a receiving step
// is the matched message, which lives in the probe outcome and not in the
// expectation result. The count carries the field that SDD section 4.6 writes
// into step.fail, and only this loop knows it.
//
// stepDeadline is the per-step ceiling of SDD section 6.3, or the zero time
// when the step names none.
func executeWithRetryProbe(ctx context.Context, step *scenario.Step, rc *RunContext, defaults config.DefaultsConfig, stepDeadline time.Time, read probeRead) (*expect.Result, *ProbeResult, int, error) {
	timeout, interval := resolveRetryConfig(step, defaults)

	if timeout == 0 {
		result, probe, err := executeOnce(ctx, step, rc, read)
		return result, probe, 1, err
	}

	deadline := time.Now().Add(timeout)
	timeoutCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var last attempt
	// attempts counts the evaluations that finished, which is the number SDD
	// section 4.6 reports. started counts the probes, and it covers the one case
	// where the two differ: a window too short to finish even one probe.
	attempts, started := 0, 0
	for {
		current, err := executeAttempt(timeoutCtx, step, rc, read)
		started++
		if err != nil {
			return nil, nil, started, err
		}
		// The window closing during a request is the window doing its job, not
		// the target going away. Keeping that attempt would report an
		// unreachable target at exit 3, when SDD section 4.6 reports a retry
		// window that closed as an assertion failure with its attempt count.
		// That attempt also evaluated nothing, so it is not one of the attempts.
		if !closedByWindow(ctx, timeoutCtx, stepDeadline, current) {
			last = current
			attempts++
		}
		if current.result.Pass {
			return current.result, current.probe, attempts, nil
		}

		select {
		case <-timeoutCtx.Done():
			// A window too short to finish one probe still tried, so the count
			// is never 0 while the loop ran.
			if attempts == 0 {
				attempts = started
			}
			// A target that stayed unreachable for the whole retry window is an
			// environment problem, not an assertion that happened to fail.
			if last.transport != nil {
				return nil, nil, attempts, last.transport
			}
			if last.result != nil {
				return last.result, last.probe, attempts, nil
			}
			return &expect.Result{
				Pass:    false,
				Message: fmt.Sprintf("retry timeout after %s", timeout),
			}, nil, attempts, nil
		case <-time.After(interval):
		}
	}
}

// closedByWindow reports whether a window the scenario set cut this attempt
// short. Two windows count: the retry window of SDD section 6.3 and the
// per-step ceiling of the same section. SDD section 4.6 reports either one
// closing as an assertion failure with its attempt count.
//
// It says no when the caller cancelled the run, because that deadline belongs
// to the run and SDD section 4.4 gives it its own exit code. The step ceiling
// is tested first, because Execute layers it onto the same context and the run
// deadline alone cannot be told from it.
func closedByWindow(ctx, timeoutCtx context.Context, stepDeadline time.Time, current attempt) bool {
	if current.transport == nil {
		return false
	}
	if !errors.Is(current.transport, context.DeadlineExceeded) {
		return false
	}
	if !stepDeadline.IsZero() && !time.Now().Before(stepDeadline) {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	return timeoutCtx.Err() != nil
}

// resolveRetryConfig returns the retry window for a step. The fallbacks repeat
// the defaults of SDD section 5.1, so a caller that passes a zero
// DefaultsConfig still gets the documented 30s window and 250ms interval.
func resolveRetryConfig(step *scenario.Step, defaults config.DefaultsConfig) (time.Duration, time.Duration) {
	timeout := defaults.AwaitTimeout.Duration()
	if timeout == 0 {
		timeout = config.DefaultAwaitTimeout
	}
	interval := defaults.AwaitInterval.Duration()
	if interval == 0 {
		interval = config.DefaultAwaitInterval
	}

	if step.Retry != nil {
		return parseDurationOrDefault(step.Retry.Timeout, timeout),
			parseDurationOrDefault(step.Retry.Interval, interval)
	}

	if step.Type == "await" {
		return timeout, interval
	}

	return 0, 0
}

func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// executeOnce runs a probe with no retry. A target it cannot reach is returned
// as an error, so SDD section 14 holds: a step that cannot reach its target
// exits 3 and not 1.
func executeOnce(ctx context.Context, step *scenario.Step, rc *RunContext, read probeRead) (*expect.Result, *ProbeResult, error) {
	current, err := executeAttempt(ctx, step, rc, read)
	if err != nil {
		return nil, nil, err
	}
	if current.transport != nil {
		return nil, nil, current.transport
	}
	return current.result, current.probe, nil
}

// attempt holds the outcome of one probe. It keeps a failure to reach the
// target apart from an expectation the target did not meet, because the two
// carry different exit codes and only the first is worth retrying on its own.
type attempt struct {
	result    *expect.Result
	probe     *ProbeResult
	transport error
}

// executeAttempt runs one probe and evaluates the expectation against it.
func executeAttempt(ctx context.Context, step *scenario.Step, rc *RunContext, read probeRead) (attempt, error) {
	probeResult, err := read(ctx, step, rc)
	if err != nil {
		return attempt{}, fmt.Errorf("probe execution: %w", err)
	}

	if probeResult.Err != nil {
		transport := fmt.Errorf("probe %q cannot reach its target: %w", step.Name, probeResult.Err)
		return attempt{
			result:    &expect.Result{Pass: false, Message: transport.Error()},
			probe:     probeResult,
			transport: transport,
		}, nil
	}

	response := buildEvalResponse(probeResult)
	result := expect.Evaluate(step.Expect, response)
	return attempt{result: &result, probe: probeResult}, nil
}

// buildEvalResponse hands the evaluator the whole probe outcome. Collapsing it
// to one value would silently drop every expectation key but the first, and SDD
// section 6.5 lets one expect combine several.
func buildEvalResponse(pr *ProbeResult) expect.Response {
	response := expect.Response{Source: pr.Source, StatusCode: pr.StatusCode, Body: pr.Response}
	if arr, ok := pr.Response.([]any); ok {
		count := len(arr)
		response.Count = &count
	}
	return response
}
