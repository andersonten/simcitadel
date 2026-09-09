package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/diagnostics"
	"github.com/andersonten/simcitadel/internal/expect"
	"github.com/andersonten/simcitadel/internal/scenario"
	"github.com/andersonten/simcitadel/internal/steps"
)

// RunConfig holds the parameters for a single run.
type RunConfig struct {
	ScenarioPaths []string
	Config        *config.CitadelConfig
	Output        string
	JUnitPath     string
	Verbose       bool
	Timeout       time.Duration
	KeepGoing     bool
	SetVars       map[string]string
	BundleDir     string
	StepThrough   bool
}

// StepRecord holds the result of one step or assertion for reporting.
// The record feeds both the JUnit report and the steps.jsonl file of SDD
// section 12.1, so it carries the inputs, the outputs and the timings.
type StepRecord struct {
	Name           string
	ClassName      string
	Type           string
	Index          int
	StartedAt      time.Time
	DurationSecs   float64
	Passed         bool
	FailureMessage string
	FailureClass   string
	Failure        map[string]any
	Attempts       int
	Retry          map[string]any
	Input          map[string]any
	Output         any
}

// bundleRecord renders one step record as the steps.jsonl line of SDD section
// 12.1, which holds the inputs, the outputs, the attempts and the timings.
func (r StepRecord) bundleRecord() map[string]any {
	record := map[string]any{
		"scenario":   r.ClassName,
		"index":      r.Index,
		"name":       r.Name,
		"type":       r.Type,
		"startedAt":  r.StartedAt.UTC().Format(bundleTimeLayout),
		"durationMs": math.Round(r.DurationSecs*1e6) / 1e3,
		"pass":       r.Passed,
	}
	// SDD section 12.1 holds the attempts of every step. A step whose count is
	// unknown records null, because a wrong count is worse than an honest gap.
	if r.Attempts > 0 {
		record["attempts"] = r.Attempts
	} else {
		record["attempts"] = nil
	}
	if len(r.Retry) > 0 {
		record["retry"] = r.Retry
	}
	if len(r.Input) > 0 {
		record["input"] = r.Input
	}
	if r.Output != nil {
		record["output"] = r.Output
	}
	if len(r.Failure) > 0 {
		failure := make(map[string]any, len(r.Failure)+1)
		for k, v := range r.Failure {
			failure[k] = v
		}
		if r.FailureClass != "" {
			failure["class"] = r.FailureClass
		}
		record["failure"] = failure
	}
	return record
}

// runState carries what phase 12 of SDD section 4.2 needs out of the phases
// that come before it. Phase 12 runs from a deferred call, including on panic,
// so it cannot read a local variable of a phase that never returned.
type runState struct {
	scenarios []*scenario.Scenario
	records   []StepRecord

	// teardown holds the release of the environment lock and the close of every
	// database connection. It runs after the bundle write, because a bundle
	// collected without the lock can read an environment that a second run has
	// already changed.
	teardown []func()
}

// Run executes one or more scenarios and returns the exit code.
func Run(ctx context.Context, rc RunConfig) (exitCode int) {
	startedAt := time.Now()
	runID := NewRunID()
	emitter := NewEmitter(os.Stdout, rc.Output, runID)
	state := &runState{}

	// SDD 4.2 phase 12 is marked "Always" and it runs even on panic, so the
	// after block, the bundle and the lock release reach a deferred call. The
	// recover turns a bug in Citadel into the exit 6 of SDD 4.4, in place of a
	// process that aborts with a stack trace and writes no diagnostics.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in the run lifecycle",
				"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			exitCode = ExitInternal
		}
		exitCode = finishRun(ctx, rc, runID, emitter, state, startedAt, exitCode)
		for i := len(state.teardown) - 1; i >= 0; i-- {
			state.teardown[i]()
		}
	}()

	return runPhases(ctx, rc, runID, emitter, state)
}

// runPhases performs phases 1 to 11 of SDD section 4.2 and returns the exit
// code they produce. Phase 12 belongs to Run, because it must run even when a
// phase here panics.
func runPhases(ctx context.Context, rc RunConfig, runID string, emitter *Emitter, state *runState) int {
	if rc.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rc.Timeout)
		state.teardown = append(state.teardown, cancel)
	}

	scenarios, err := loadAndValidate(rc)
	if err != nil {
		slog.Error("validation failed", "error", err.Error())
		return ClassifyError(err)
	}
	state.scenarios = scenarios

	if err := requireBrokerConfig(rc.Config); err != nil {
		slog.Error("broker configuration is incomplete", "error", err.Error())
		return ClassifyError(err)
	}

	lock, err := acquireLock(ctx, rc.Config.Targets.RabbitMQ.AMQPURL, rc.Config.Environment.Name)
	if err != nil {
		slog.Error("lock acquisition failed", "error", err.Error())
		return ClassifyError(err)
	}
	state.teardown = append(state.teardown, func() {
		if releaseErr := lock.Release(); releaseErr != nil {
			slog.Error("lock release failed", "error", releaseErr.Error())
		}
	})
	slog.Info("environment lock acquired", "environment", rc.Config.Environment.Name)

	httpClient := &http.Client{Timeout: 30 * time.Second}

	// The connections open before phase 4, because the SELECT 1 of SDD
	// section 4.10 needs them. Only the referenced targets open, for the same
	// reason that the preflight checks only the referenced targets.
	refs := referencedTargets(scenarios, rc.Config)
	databases, closeDatabases, err := openDatabaseTargets(ctx, rc.Config, refs.databases)
	if err != nil {
		slog.Error("database connection failed", "error", err.Error())
		return ClassifyError(err)
	}
	state.teardown = append(state.teardown, closeDatabases)

	if err := Preflight(ctx, rc.Config, httpClient, scenarios, databases); err != nil {
		slog.Error("preflight failed", "error", err.Error())
		return ClassifyError(err)
	}

	// The WireMock pack loads once, after the preflight proves the server
	// answers and before the first scenario. It does not load per scenario,
	// because the reset of SDD section 4.9 returns every scenario to its start
	// state and leaves the mappings in place.
	if err := LoadWireMockStubs(ctx, rc.Config, httpClient); err != nil {
		slog.Error("wiremock stub load failed", "error", err.Error())
		return ClassifyError(err)
	}

	tokens, err := AcquireToken(ctx, rc.Config, httpClient)
	if err != nil {
		slog.Error("token acquisition failed", "error", err.Error())
		return ClassifyError(err)
	}
	if tokens.cached() != "" {
		slog.Info("bearer token acquired")
	}

	emitter.RunStart(scenarioNames(scenarios), rc.Config.Environment.Name)

	exitCode := ExitSuccess

	for _, sc := range scenarios {
		// SDD 4.9: reset is unconditional and runs before every scenario, with
		// that scenario's own truncate list. Merging the lists of every scenario
		// into one reset would let the first scenario run against the last
		// scenario's fixtures.
		var truncate []string
		if sc.Reset != nil {
			truncate = sc.Reset.Truncate
		}
		if err := Reset(ctx, rc.Config, runID, httpClient, databases, truncate...); err != nil {
			slog.Error("reset failed", "error", err.Error(), "scenario", sc.Name)
			return mergeExitCode(exitCode, ClassifyError(err))
		}

		// The baseline is read after reset, because reset can truncate the outbox
		// table and a truncate is not an operation the check reports. The counters
		// are cumulative for the life of the statistics, so only a delta against
		// this value describes the run window.
		outboxBaseline, err := ReadOutboxOps(ctx, rc.Config, databases)
		if err != nil {
			slog.Error("read the outbox baseline failed", "error", err.Error(), "scenario", sc.Name)
			return mergeExitCode(exitCode, ClassifyError(err))
		}

		code, records := runScenario(ctx, sc, rc, emitter, runID, tokens, databases, outboxBaseline)
		state.records = append(state.records, records...)
		if code != ExitSuccess {
			exitCode = mergeExitCode(exitCode, code)
			if !rc.KeepGoing {
				break
			}
		}
	}

	return exitCode
}

// requireBrokerConfig holds the two broker addresses that every run needs.
//
// SDD section 4.8 makes one run at a time unconditional, so a run without a
// broker to hold the lock on cannot proceed. Step 3 of SDD 4.9 purges every
// queue through the management API, step 7 verifies the depths there, and the
// unroutable check of SDD 4.11 reads the same API. A configuration that names
// no management URL leaves all three out in silence, and a run whose message
// reached the unroutable queue then reports a pass.
func requireBrokerConfig(cfg *config.CitadelConfig) error {
	if cfg.Targets.RabbitMQ == nil || cfg.Targets.RabbitMQ.AMQPURL == "" {
		return &InvalidInputError{Msg: "targets.rabbitmq.amqpUrl is required, because the environment lock lives on the broker"}
	}
	if cfg.Targets.RabbitMQ.ManagementURL == "" {
		return &InvalidInputError{Msg: "targets.rabbitmq.managementUrl is required, " +
			"because the reset purges every queue through it and the unroutable check reads it"}
	}
	return nil
}

// finishRun performs phase 12 of SDD section 4.2 for the whole run. It writes
// the JUnit report, writes the diagnostic bundle on any non-zero exit as SDD
// section 12.1 requires, and emits the run.end event of section 4.6.
func finishRun(ctx context.Context, rc RunConfig, runID string, emitter *Emitter, state *runState, startedAt time.Time, exitCode int) int {
	if ctx.Err() == context.DeadlineExceeded {
		exitCode = ExitTimeout
	}

	if rc.JUnitPath != "" {
		writeJUnitFromRecords(state.records, rc.JUnitPath)
	}

	outcome := "pass"
	if exitCode != ExitSuccess {
		outcome = "fail"
	}

	bundlePath := ""
	if exitCode != ExitSuccess && rc.BundleDir != "" && rc.Config != nil {
		bc := collectBundleData(ctx, rc, runID, state.scenarios, state.records)
		bc.Config = runReport(rc, runID, outcome, exitCode, startedAt, time.Now(), state.scenarios)
		if bundleErr := writeBundle(bc); bundleErr != nil {
			slog.Error("write diagnostic bundle failed", "error", bundleErr.Error())
		} else {
			bundlePath = filepath.Join(rc.BundleDir, runID) + string(os.PathSeparator)
		}
	}

	emitter.RunEnd(outcome, exitCode, bundlePath)
	return exitCode
}

// scenarioNames joins the names of every scenario in the run for the run.start event.
func scenarioNames(scenarios []*scenario.Scenario) string {
	names := make([]string, len(scenarios))
	for i, sc := range scenarios {
		names[i] = sc.Name
	}
	return strings.Join(names, ",")
}

// loadAndValidate parses, validates and renders every scenario the run names.
// It collects every problem across every file before it returns, because SDD
// section 4.2 phase 2 validates the whole run and a developer who fixes one
// error at a time pays one round trip per error.
func loadAndValidate(rc RunConfig) ([]*scenario.Scenario, error) {
	paths, err := scenario.ExpandPaths(rc.ScenarioPaths)
	if err != nil {
		return nil, &InvalidInputError{Msg: err.Error()}
	}

	var result []*scenario.Scenario
	var problems []string

	for _, path := range paths {
		sc, err := scenario.Parse(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("parse scenario %s: %v", path, err))
			continue
		}

		basePath := filepath.Dir(path)
		errs := scenario.ValidateWithOptions(sc, scenario.Options{BasePath: basePath, Config: rc.Config})
		if len(errs) > 0 {
			for _, e := range errs {
				problems = append(problems, fmt.Sprintf("validate scenario %s: %v", path, e))
			}
			continue
		}

		vars := mergeVariables(sc.Variables, rc.SetVars)
		rendered, err := scenario.RenderMetadata(sc, vars)
		if err != nil {
			problems = append(problems, fmt.Sprintf("render scenario %s: %v", path, err))
			continue
		}
		rendered.Variables = vars

		result = append(result, rendered)
	}

	if len(problems) > 0 {
		return nil, &InvalidInputError{Msg: strings.Join(problems, "\n")}
	}
	return result, nil
}

func mergeVariables(base map[string]any, overrides map[string]string) map[string]any {
	vars := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		vars[k] = v
	}
	for k, v := range overrides {
		vars[k] = v
	}
	return vars
}

func runScenario(ctx context.Context, sc *scenario.Scenario, rc RunConfig, emitter *Emitter, runID string, tokens *TokenSource, databases map[string]*sql.DB, outboxBaseline OutboxOps) (exitCode int, records []StepRecord) {
	exitCode = ExitSuccess

	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in scenario execution", "panic", fmt.Sprintf("%v", r), "scenario", sc.Name)
			exitCode = ExitInternal
			if rc.BundleDir != "" {
				bundleErr := writeBundle(diagnostics.BundleConfig{
					BundleDir: rc.BundleDir,
					RunID:     runID,
					Scenario:  scenariosYAML([]*scenario.Scenario{sc}),
					Steps:     bundleRecords(records),
				})
				if bundleErr != nil {
					slog.Error("write diagnostic bundle after panic failed", "error", bundleErr.Error())
				}
			}
		}
	}()

	runCtx := steps.NewRunContext(rc.Config, runID)
	runCtx.StepThrough = rc.StepThrough
	runCtx.DBs = databases
	runCtx.Variables = sc.Variables

	defer func() {
		for i := range sc.After {
			afterStep := &sc.After[i]
			_, afterErr := executeStep(ctx, afterStep, runCtx, tokens)
			if afterErr != nil {
				slog.Warn("after step failed", "step", afterStep.Name, "error", afterErr.Error())
			}
		}
	}()

	stepFailed := false

	for i := range sc.Steps {
		step := &sc.Steps[i]
		if ctx.Err() == context.DeadlineExceeded {
			exitCode = ExitTimeout
			break
		}

		idx := i + 1
		emitter.StepStart(idx, step.Name, step.Type)

		started := time.Now()
		result, err := executeStep(ctx, step, runCtx, tokens)
		record := recordStep(sc, step, idx, started, result, err)
		records = append(records, record)

		if record.Passed {
			emitter.StepPass(idx, int64(record.DurationSecs*1000))
			continue
		}

		emitter.StepFail(idx, int64(record.DurationSecs*1000), record.Failure, record.FailureClass)
		if step.Optional {
			slog.Warn("optional step failed, continuing", "step", step.Name, "error", record.FailureMessage)
			continue
		}
		if err != nil {
			exitCode = mergeExitCode(exitCode, ClassifyError(err))
		} else {
			exitCode = mergeExitCode(exitCode, ExitFailure)
		}
		stepFailed = true
		break
	}

	// SDD 4.2 phase 9: every assertion runs and reports, so one run shows the
	// full picture. A failed step stops the remaining steps only.
	for i := range sc.Assertions {
		assertion := &sc.Assertions[i]
		if ctx.Err() == context.DeadlineExceeded {
			exitCode = ExitTimeout
			break
		}

		idx := len(sc.Steps) + i + 1
		emitter.StepStart(idx, assertion.Name, assertion.Type)

		started := time.Now()
		result, err := executeStep(ctx, assertion, runCtx, tokens)
		record := recordStep(sc, assertion, idx, started, result, err)
		records = append(records, record)

		if record.Passed {
			emitter.StepPass(idx, int64(record.DurationSecs*1000))
			continue
		}

		emitter.StepFail(idx, int64(record.DurationSecs*1000), record.Failure, record.FailureClass)
		if err != nil {
			exitCode = mergeExitCode(exitCode, ClassifyError(err))
			continue
		}
		exitCode = mergeExitCode(exitCode, ExitFailure)
	}

	failures := endOfRunChecks(ctx, rc.Config, &http.Client{Timeout: 10 * time.Second}, databases, outboxBaseline)
	exitCode = mergeExitCode(exitCode, reportEndOfRunFailures(emitter, failures))

	if stepFailed && exitCode == ExitSuccess {
		exitCode = ExitFailure
	}

	return exitCode, records
}

// executeStep renews the bearer token and runs one step. SDD section 9
// refreshes the token 60 s before expiry, and the step layer reads the token
// from the run context, so the renewal belongs here.
func executeStep(ctx context.Context, step *scenario.Step, runCtx *steps.RunContext, tokens *TokenSource) (*steps.StepResult, error) {
	if tokens != nil {
		token, err := tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		runCtx.BearerToken = token
	}
	return steps.Execute(ctx, step, runCtx)
}

// recordStep builds the report record of one executed step.
func recordStep(sc *scenario.Scenario, step *scenario.Step, index int, started time.Time, result *steps.StepResult, err error) StepRecord {
	// A step that errors still consumed wall clock time. Reporting 0 here
	// would hide a step that spent its whole timeout before it failed.
	duration := time.Since(started)
	if result != nil {
		duration = result.Duration
	}

	record := StepRecord{
		Name:         step.Name,
		ClassName:    sc.Name,
		Type:         step.Type,
		Index:        index,
		StartedAt:    started,
		DurationSecs: duration.Seconds(),
		Input:        compactMap(structMap(step)),
	}

	if step.Retry != nil {
		record.Retry = compactMap(structMap(step.Retry))
	}
	// The retry loop of internal/steps counts its own evaluations, which is the
	// attempts field of SDD sections 4.6 and 12.1. A step that ran once reports
	// 1. A step that could not run at all leaves result nil.
	if result != nil {
		record.Attempts = result.Attempts
	}

	switch {
	case err != nil:
		record.FailureClass = classifyStepFailure(err)
		record.FailureMessage = err.Error()
		record.Failure = map[string]any{"message": err.Error()}
	case result.Pass:
		record.Passed = true
		record.Output = stepOutput(result)
	default:
		record.Failure = buildFailureMap(result)
		record.FailureClass = "assertion"
		record.FailureMessage = failureMessage(record.Failure)
		record.Output = stepOutput(result)
	}

	if record.Failure != nil {
		addFailureContext(record.Failure, step, record.Attempts)
	}

	return record
}

// addFailureContext adds the step context that the step.fail schema of SDD
// section 4.6 names. The jsonPath comes from the expectation that the step
// declares. The attempt count stays out when it is unknown, because a wrong
// count is worse than an absent one.
func addFailureContext(failure map[string]any, step *scenario.Step, attempts int) {
	if path := expectJSONPath(step); path != "" {
		failure["jsonPath"] = path
	}
	if attempts > 0 {
		failure["attempts"] = attempts
	}
}

// expectJSONPath returns the path that the expectation of a step reads. A map
// of several paths returns nothing, because the runner cannot tell which of
// them failed and a wrong path sends a reader to the wrong place.
func expectJSONPath(step *scenario.Step) string {
	if step == nil || step.Expect == nil {
		return ""
	}
	switch path := step.Expect.JSONPath.(type) {
	case string:
		return path
	case map[string]any:
		if len(path) == 1 {
			for key := range path {
				return key
			}
		}
	}
	return step.Expect.JSONPathExists
}

// stepOutput returns what the step produced, as a value that the redaction of
// SDD section 12.2 can walk.
func stepOutput(result *steps.StepResult) any {
	if result.Binding != nil {
		return jsonValue(result.Binding)
	}
	if result.Expect != nil {
		return jsonValue(result.Expect.Actual)
	}
	return nil
}

func buildFailureMap(result *steps.StepResult) map[string]any {
	failure := make(map[string]any)
	if result.Err != nil {
		failure["message"] = result.Err.Error()
	}
	if result.Expect != nil && result.Expect.Message != "" {
		failure["message"] = result.Expect.Message
	}
	if result.Expect != nil {
		if result.Expect.Expected != nil {
			failure["expected"] = result.Expect.Expected
		}
		if result.Expect.Actual != nil {
			failure["actual"] = result.Expect.Actual
		}
	}
	return failure
}

func failureMessage(failure map[string]any) string {
	if m, ok := failure["message"]; ok {
		return fmt.Sprintf("%v", m)
	}
	return ""
}

// classifyStepFailure names the failure class of SDD section 4.4. The class
// must agree with the exit code the same error produces, because the two are
// the same taxonomy read two ways.
func classifyStepFailure(err error) string {
	switch ClassifyError(err) {
	case ExitSuccess:
		return ""
	case ExitFailure:
		return "assertion"
	case ExitInvalidInput:
		return "input"
	case ExitEnvironment:
		return "environment"
	case ExitTimeout:
		return "timeout"
	default:
		return "internal"
	}
}

// checkFailure names the end-of-run check that failed and carries its error.
// The name reaches the JSON event stream, so a consumer of --output json reads
// which check ended the run and why.
type checkFailure struct {
	Check string
	Err   error
}

// The names that the check.fail event of SDD section 4.11 carries.
const (
	checkNameUnroutable    = "unroutable-depth"
	checkNameUnconsumed    = "unconsumed-depth"
	checkNameConformance   = "envelope-conformance"
	checkNameSkippedOutbox = "skipped-outbox"
	checkNameUnmatched     = "unmatched-requests"
)

// endOfRunChecks performs the environment verification of SDD section 4.11
// after the assertions. Every check runs, so one run reports every problem.
func endOfRunChecks(ctx context.Context, cfg *config.CitadelConfig, httpClient *http.Client, databases map[string]*sql.DB, outboxBaseline OutboxOps) []checkFailure {
	var failures []checkFailure
	record := func(name string, err error) {
		if err != nil {
			failures = append(failures, checkFailure{Check: name, Err: err})
		}
	}

	if cfg.Targets.RabbitMQ != nil && cfg.Targets.RabbitMQ.ManagementURL != "" {
		record(checkNameUnroutable, checkUnroutableDepth(ctx, cfg.Targets.RabbitMQ, httpClient))
		record(checkNameUnconsumed, checkUnconsumedDepth(ctx, cfg, httpClient))
	}
	record(checkNameConformance, checkConformance(ctx, cfg, httpClient))
	record(checkNameSkippedOutbox, CheckSkippedOutbox(ctx, cfg, databases, outboxBaseline))
	record(checkNameUnmatched, checkUnmatchedRequests(ctx, cfg, httpClient))
	return failures
}

// reportEndOfRunFailures logs and emits every end-of-run failure, and returns
// the exit code the failures carry. SDD section 4.4 keeps exit 1 for a finding
// about the application, so a malformed envelope exits 1 while a broken
// topology, an unconsumed queue and an unmatched request exit 3. A finding
// about the application wins over an environment problem, because that finding
// is direct evidence about the system under test.
func reportEndOfRunFailures(emitter *Emitter, failures []checkFailure) int {
	code := ExitSuccess
	for _, failure := range failures {
		class := classifyStepFailure(failure.Err)
		slog.Error("end-of-run check failed",
			"check", failure.Check, "class", class, "error", failure.Err.Error())
		emitter.CheckFail(failure.Check, class, failure.Err.Error())

		code = mergeExitCode(code, ClassifyError(failure.Err))
	}
	return code
}

// readCheckJSON reads the JSON answer of one end-of-run check into out, and
// returns the status the target reported. A check that cannot reach its target,
// that reads a non-2xx status, or that cannot decode the answer proves nothing,
// so every one of those is an environment problem of SDD section 4.4. SDD
// section 4.11 makes these checks the only guard that a scenario author does
// not write, so a silent pass here hides a real defect.
func readCheckJSON(ctx context.Context, client *http.Client, check, method, target string, body []byte, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return 0, &EnvironmentError{Msg: fmt.Sprintf("%s check: build request: %v", check, err)}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return runCheckRequest(client, check, req, out)
}

// readManagementJSON is readCheckJSON against the RabbitMQ management API, which
// needs the credentials of SDD section 5.1.
func readManagementJSON(ctx context.Context, client *http.Client, check string, mgmt management,
	method, path string, body []byte, out any) (int, error) {
	req, err := mgmt.request(ctx, method, path, body)
	if err != nil {
		return 0, &EnvironmentError{Msg: fmt.Sprintf("%s check: build request: %v", check, err)}
	}
	return runCheckRequest(client, check, req, out)
}

// runCheckRequest holds the shared body of the two readers above.
func runCheckRequest(client *http.Client, check string, req *http.Request, out any) (int, error) {
	target := req.URL.String()

	resp, err := client.Do(req)
	if err != nil {
		return 0, &EnvironmentError{Msg: fmt.Sprintf("%s check: cannot reach %s: %v", check, redactURL(target), err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &EnvironmentError{Msg: fmt.Sprintf(
			"%s check: %s returned status %d, expect 2xx", check, redactURL(target), resp.StatusCode)}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, &EnvironmentError{Msg: fmt.Sprintf(
			"%s check: cannot decode the answer of %s: %v", check, redactURL(target), err)}
	}
	return resp.StatusCode, nil
}

// redactURL removes the credentials that a management URL carries, so a log
// line and a failure message never hold a password.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}

// unroutableSampleLimit bounds how many messages the failure names. A failure
// that names ten messages stays readable.
const unroutableSampleLimit = 10

// checkUnroutableDepth fails the run when the unroutable queue of SDD section
// 7.4 holds a message, and names the exchange and the routing key of each one,
// as SDD section 4.11 requires.
func checkUnroutableDepth(ctx context.Context, target *config.RabbitMQTarget, client *http.Client) error {
	mgmt := managementFor(target)
	queue := citadelamqp.UnroutableQueueName()

	var queueInfo struct {
		Messages int `json:"messages"`
	}
	status, err := readManagementJSON(ctx, client, checkNameUnroutable, mgmt, http.MethodGet,
		fmt.Sprintf("/api/queues/%%2F/%s", url.PathEscape(queue)), nil, &queueInfo)
	if status == http.StatusNotFound {
		return &EnvironmentError{Msg: fmt.Sprintf(
			"%s check: queue %s is not declared, so an unroutable message has nowhere to land", checkNameUnroutable, queue)}
	}
	if err != nil {
		return err
	}
	if queueInfo.Messages == 0 {
		return nil
	}

	routes, routesErr := unroutableRoutes(ctx, client, mgmt, queue, queueInfo.Messages)
	if routesErr != nil {
		return &EnvironmentError{Msg: fmt.Sprintf(
			"queue %s holds %d message(s), and the exchange and routing key of each message cannot be read: %v",
			queue, queueInfo.Messages, routesErr)}
	}
	return &EnvironmentError{Msg: fmt.Sprintf(
		"queue %s holds %d message(s): %s", queue, queueInfo.Messages, strings.Join(routes, "; "))}
}

// unroutableRoutes names the exchange and the routing key of each message in
// the unroutable queue. The read is non-destructive, because ack_requeue_true
// puts every message back on the queue.
func unroutableRoutes(ctx context.Context, client *http.Client, mgmt management, queue string, depth int) ([]string, error) {
	count := depth
	if count > unroutableSampleLimit {
		count = unroutableSampleLimit
	}
	body, err := json.Marshal(map[string]any{
		"count":    count,
		"ackmode":  "ack_requeue_true",
		"encoding": "auto",
		"truncate": 50000,
	})
	if err != nil {
		return nil, fmt.Errorf("build the message read request: %w", err)
	}

	var messages []struct {
		Exchange   string `json:"exchange"`
		RoutingKey string `json:"routing_key"`
	}
	if _, err := readManagementJSON(ctx, client, checkNameUnroutable, mgmt, http.MethodPost,
		fmt.Sprintf("/api/queues/%%2F/%s/get", url.PathEscape(queue)), body, &messages); err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("the management API returned no message for a queue at depth %d", depth)
	}

	routes := make([]string, 0, len(messages)+1)
	for _, message := range messages {
		exchange := message.Exchange
		if exchange == "" {
			exchange = "(default)"
		}
		routes = append(routes, fmt.Sprintf("exchange %s, routing key %s", exchange, message.RoutingKey))
	}
	if depth > len(messages) {
		routes = append(routes, fmt.Sprintf("and %d more message(s)", depth-len(messages)))
	}
	return routes, nil
}

// checkUnconsumedDepth reports any app-consumed queue that still holds messages.
// Citadel's own queues are excluded, because a spy queue holds every message it
// captured by design and the unroutable queue has its own check.
// The result is a warning by default and an error when strictUnconsumed is set.
// A management API that does not answer is an environment problem, because a
// check that reads nothing proves nothing.
func checkUnconsumedDepth(ctx context.Context, cfg *config.CitadelConfig, client *http.Client) error {
	depths, err := queueDepths(ctx, cfg.Targets.RabbitMQ, client)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("%s check: cannot list the queues of %s: %v",
			checkNameUnconsumed, redactURL(cfg.Targets.RabbitMQ.ManagementURL), err)}
	}

	var unconsumed []string
	for _, q := range depths {
		if citadelamqp.IsCitadelQueue(q.Name) || q.Messages == 0 {
			continue
		}
		unconsumed = append(unconsumed, fmt.Sprintf("%s (depth %d)", q.Name, q.Messages))
	}
	if len(unconsumed) == 0 {
		return nil
	}

	sort.Strings(unconsumed)
	if cfg.Defaults.StrictUnconsumed {
		return &EnvironmentError{Msg: fmt.Sprintf("unconsumed messages remain in %s", strings.Join(unconsumed, ", "))}
	}
	slog.Warn("unconsumed messages remain", "queues", strings.Join(unconsumed, ", "))
	return nil
}

// checkConformance validates every captured message against CloudEvents 1.0 plus the
// required version attribute. Messages on ignore-listed exchanges are exempt.
// A malformed envelope is a finding about the application, so it carries the
// assertion class and exit 1 of SDD section 4.4. A capture service that does
// not answer is an environment problem, so it carries exit 3.
// CheckConformance runs the envelope conformance check of SDD section 4.11
// against the capture service the config names. The integration suite of SDD
// section 14.2 needs the real check, because a test that repeats the rule
// proves the rule and not the wiring that applies it.
func CheckConformance(ctx context.Context, cfg *config.CitadelConfig, client *http.Client) error {
	return checkConformance(ctx, cfg, client)
}

func checkConformance(ctx context.Context, cfg *config.CitadelConfig, client *http.Client) error {
	if cfg.Targets.EventCapture == nil || cfg.Targets.EventCapture.URL == "" {
		return nil
	}
	captureURL := strings.TrimRight(cfg.Targets.EventCapture.URL, "/") + "/captured"

	var messages []map[string]any
	if _, err := readCheckJSON(ctx, client, checkNameConformance, http.MethodGet, captureURL, nil, &messages); err != nil {
		return err
	}

	ignoreSet := make(map[string]bool, len(cfg.Capture.Ignore))
	for _, ex := range cfg.Capture.Ignore {
		ignoreSet[ex] = true
	}

	// Every malformed message is reported, not only the first. A run that
	// publishes one bad envelope usually publishes several, and a check that
	// stops at the first sends the developer back for another run per message.
	var violations []string
	for _, msg := range messages {
		exchange, _ := msg["exchange"].(string)
		if ignoreSet[exchange] {
			continue
		}
		if _, ok := msg["body"].(map[string]any); !ok {
			violations = append(violations, fmt.Sprintf("%s on exchange %s has no JSON body",
				messageLabel(msg), exchange))
			continue
		}
		if err := expect.ValidateCloudEvent(expect.CloudEventEnvelope(msg)); err != nil {
			violations = append(violations, fmt.Sprintf("%s on exchange %s: %v",
				messageLabel(msg), exchange, err))
		}
	}
	if len(violations) > 0 {
		return &StepFailureError{Msg: "conformance: " + strings.Join(violations, "; ")}
	}
	return nil
}

// messageLabel names a captured message the way a developer can find it again.
// A slice index is useless once the buffer holds another run, so the label
// prefers the event id and then the sequence number that SDD section 8.2 stores
// with each entry.
func messageLabel(msg map[string]any) string {
	if body, ok := msg["body"].(map[string]any); ok {
		if id, ok := body["id"].(string); ok && id != "" {
			return fmt.Sprintf("event %s", id)
		}
	}
	// A JSON number decodes as a float64 through an any.
	if seq, ok := msg["seq"].(float64); ok {
		return fmt.Sprintf("captured message seq %d", int64(seq))
	}
	if routingKey, ok := msg["routingKey"].(string); ok && routingKey != "" {
		return fmt.Sprintf("captured message with routing key %s", routingKey)
	}
	return "captured message"
}

// checkUnmatchedRequests reports WireMock unmatched requests with near-misses.
// The result is a warning by default and an error when strictUnmatched is set.
// A WireMock that does not answer is an environment problem, because the check
// then holds no evidence that the run matched every request.
func checkUnmatchedRequests(ctx context.Context, cfg *config.CitadelConfig, client *http.Client) error {
	if cfg.Targets.WireMock == nil || cfg.Targets.WireMock.AdminURL == "" {
		return nil
	}
	reqURL := cfg.Targets.WireMock.AdminBase() + "/requests/unmatched"

	var result struct {
		Requests []any `json:"requests"`
	}
	if _, err := readCheckJSON(ctx, client, checkNameUnmatched, http.MethodGet, reqURL, nil, &result); err != nil {
		return err
	}

	if len(result.Requests) == 0 {
		return nil
	}
	if cfg.Defaults.StrictUnmatched {
		return &EnvironmentError{Msg: fmt.Sprintf(
			"WireMock recorded %d unmatched request(s); see the near-misses in the diagnostic bundle", len(result.Requests))}
	}
	slog.Warn("WireMock recorded unmatched requests", "count", len(result.Requests))
	return nil
}

// defaultBundleRetention is the retention period of SDD section 12.1. The
// configuration file holds no bundleRetention key, so the default stands alone.
const defaultBundleRetention = 7 * 24 * time.Hour

// writeBundle writes the diagnostic bundle and then removes the bundles that
// are older than the retention period, which SDD section 12.1 does on write.
func writeBundle(bc diagnostics.BundleConfig) error {
	if err := diagnostics.WriteBundle(bc); err != nil {
		return err
	}
	if err := diagnostics.PruneBundles(bc.BundleDir, defaultBundleRetention); err != nil {
		slog.Warn("prune old diagnostic bundles failed", "error", err.Error())
	}
	return nil
}

// runReport builds the run.json of SDD section 12.1, which holds the outcome,
// the timings and the resolved configuration. WriteBundle redacts the secrets.
func runReport(rc RunConfig, runID, outcome string, exitCode int, startedAt, endedAt time.Time, scenarios []*scenario.Scenario) map[string]any {
	names := make([]string, 0, len(scenarios))
	for _, sc := range scenarios {
		if sc != nil {
			names = append(names, sc.Name)
		}
	}

	return map[string]any{
		"runId":       runID,
		"outcome":     outcome,
		"exitCode":    exitCode,
		"environment": rc.Config.Environment.Name,
		"scenarios":   names,
		"startedAt":   startedAt.UTC().Format(bundleTimeLayout),
		"endedAt":     endedAt.UTC().Format(bundleTimeLayout),
		"durationMs":  endedAt.Sub(startedAt).Milliseconds(),
		"config":      compactMap(structMap(rc.Config)),
	}
}

// bundleTimeLayout writes a timestamp with the millisecond precision that the
// event stream of SDD section 4.6 uses.
const bundleTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// bundleRecords renders every step record as one steps.jsonl line.
func bundleRecords(records []StepRecord) []map[string]any {
	if len(records) == 0 {
		return nil
	}
	lines := make([]map[string]any, len(records))
	for i, record := range records {
		lines[i] = record.bundleRecord()
	}
	return lines
}

// scenariosYAML renders the scenarios of the run as the scenario.yaml of SDD
// section 12.1, with the variables of the run resolved.
func scenariosYAML(scenarios []*scenario.Scenario) string {
	var out strings.Builder
	for _, sc := range scenarios {
		if sc == nil {
			continue
		}
		doc, err := yaml.Marshal(compactMap(structMap(sc)))
		if err != nil {
			slog.Warn("render scenario for the bundle failed", "scenario", sc.Name, "error", err.Error())
			continue
		}
		if out.Len() > 0 {
			out.WriteString("---\n")
		}
		out.Write(doc)
	}
	return out.String()
}

// structMap renders a value through YAML, so a record carries the keys that the
// scenario file and the configuration file use.
func structMap(v any) map[string]any {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// jsonValue renders a value through JSON, so the redaction of SDD section 12.2
// can walk it as maps, slices and strings.
func jsonValue(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

// compactMap drops the unset keys of a rendered document, so a record holds
// what the author wrote instead of every key of the struct.
func compactMap(m map[string]any) map[string]any {
	compacted, _ := compactValue(m).(map[string]any)
	return compacted
}

func compactValue(v any) any {
	switch value := v.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			compacted := compactValue(item)
			if isUnset(compacted) {
				continue
			}
			result[key] = compacted
		}
		return result
	case []any:
		result := make([]any, 0, len(value))
		for _, item := range value {
			result = append(result, compactValue(item))
		}
		return result
	default:
		return v
	}
}

// isUnset reports whether a rendered value carries nothing. A zero number and a
// false flag stay, because an expectation of zero rows is a real expectation.
func isUnset(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case map[string]any:
		return len(value) == 0
	case []any:
		return len(value) == 0
	default:
		return false
	}
}

// collectBundleData gathers runtime diagnostics from all targets for the bundle.
func collectBundleData(ctx context.Context, rc RunConfig, runID string, scenarios []*scenario.Scenario, records []StepRecord) diagnostics.BundleConfig {
	bc := diagnostics.BundleConfig{
		BundleDir: rc.BundleDir,
		RunID:     runID,
		Scenario:  scenariosYAML(scenarios),
		Steps:     bundleRecords(records),
	}
	client := &http.Client{Timeout: 10 * time.Second}

	if rc.Config.Targets.WireMock != nil && rc.Config.Targets.WireMock.AdminURL != "" {
		adminURL := rc.Config.Targets.WireMock.AdminBase()
		bc.WireMock = &diagnostics.WireMockDiag{
			Unmatched:  fetchJSON(ctx, client, adminURL+"/requests/unmatched"),
			NearMisses: fetchJSON(ctx, client, adminURL+"/requests/unmatched/near-misses"),
			Journal:    fetchJSON(ctx, client, adminURL+"/requests"),
		}
	}

	if rc.Config.Targets.RabbitMQ != nil && rc.Config.Targets.RabbitMQ.ManagementURL != "" {
		mgmt := managementFor(rc.Config.Targets.RabbitMQ)
		bc.RabbitMQ = &diagnostics.RabbitMQDiag{
			Queues:   fetchManagementJSON(ctx, client, mgmt, "/api/queues"),
			Topology: fetchManagementJSON(ctx, client, mgmt, "/api/exchanges"),
		}
	}

	if rc.Config.Targets.EventCapture != nil && rc.Config.Targets.EventCapture.URL != "" {
		captureURL := strings.TrimRight(rc.Config.Targets.EventCapture.URL, "/")
		raw := fetchJSON(ctx, client, captureURL+"/captured")
		if arr, ok := raw.([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					bc.Captured = append(bc.Captured, m)
				}
			}
		}
	}

	if rc.Config.Targets.DebeziumOutbox != nil && rc.Config.Targets.DebeziumOutbox.URL != "" {
		dbzURL := strings.TrimRight(rc.Config.Targets.DebeziumOutbox.URL, "/")
		if connState := fetchJSON(ctx, client, dbzURL+"/q/health"); connState != nil {
			bc.Debezium = map[string]any{"health": connState}
		}
	}

	return bc
}

func fetchJSON(ctx context.Context, client *http.Client, url string) any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	return readJSONBody(client, req)
}

// fetchManagementJSON is fetchJSON against the RabbitMQ management API. Without
// the credentials of SDD section 5.1 the two RabbitMQ files of the section 12.1
// bundle hold null, which is the moment a developer needs them most.
func fetchManagementJSON(ctx context.Context, client *http.Client, mgmt management, path string) any {
	req, err := mgmt.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil
	}
	return readJSONBody(client, req)
}

// readJSONBody decodes the answer of one best-effort diagnostic read. Every
// failure gives nil, because a bundle is written on a path that already failed.
func readJSONBody(client *http.Client, req *http.Request) any {
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var result any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	return result
}

// unnamedScenarioSuite names the suite of a record that carries no scenario
// name. A validated scenario always has a name, so this is a last resort that
// keeps the file valid.
const unnamedScenarioSuite = "citadel"

// writeJUnitFromRecords writes one test suite per scenario, as SDD section 4.6
// requires. The suite time is the sum of the times of its cases, so a pipeline
// reads the real duration of the scenario.
func writeJUnitFromRecords(records []StepRecord, path string) {
	var order []string
	byScenario := make(map[string]*JUnitReport, len(records))

	for _, rec := range records {
		name := rec.ClassName
		if name == "" {
			name = unnamedScenarioSuite
		}
		report, seen := byScenario[name]
		if !seen {
			report = &JUnitReport{Name: name}
			byScenario[name] = report
			order = append(order, name)
		}

		tc := JUnitTestCase{
			Name:      rec.Name,
			ClassName: rec.ClassName,
			Time:      rec.DurationSecs,
		}
		if !rec.Passed {
			tc.Failure = &JUnitFailure{
				Message: rec.FailureMessage,
				Type:    rec.FailureClass,
				Text:    rec.FailureMessage,
			}
			report.Failures++
		}
		report.TestCases = append(report.TestCases, tc)
		report.Tests++
		report.Time += rec.DurationSecs
	}

	reports := make([]*JUnitReport, 0, len(order))
	for _, name := range order {
		reports = append(reports, byScenario[name])
	}

	if err := WriteJUnitSuites(path, reports); err != nil {
		slog.Error("write junit report failed", "error", err.Error())
	}
}
