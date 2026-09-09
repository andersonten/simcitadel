package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/diagnostics"
	"github.com/andersonten/simcitadel/internal/runner"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// version, commit and buildDate hold the build identity of SDD section 4.1. The
// Makefile sets each one with -ldflags -X. A plain go build keeps the placeholder.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// The two output formats of SDD section 4.1.
const (
	outputHuman = "human"
	outputJSON  = "json"
)

// defaultConfigName is the configuration file that SDD section 4.1 prefers.
const defaultConfigName = "citadel.yaml"

// defaultBundleDir is the diagnostic bundle root of SDD sections 4.1 and 12.1.
const defaultBundleDir = ".citadel/runs"

// forwardReadyTimeout bounds the wait for one local port of citadel forward.
const forwardReadyTimeout = 15 * time.Second

// subcommand describes one citadel command. newFlagSet builds the flag surface
// without a side effect, so a test asserts the flags of every command.
type subcommand struct {
	name       string
	summary    string
	newFlagSet func() *flag.FlagSet
	run        func(args []string) int
}

// subcommands returns every citadel command in the order of SDD section 4.1.
func subcommands() []subcommand {
	return []subcommand{
		{
			name:       "run",
			summary:    "Execute one or more scenarios",
			newFlagSet: func() *flag.FlagSet { return newRunFlagSet(&runOptions{}) },
			run:        runCmd,
		},
		{
			name:       "validate",
			summary:    "Validate scenario files without execution",
			newFlagSet: func() *flag.FlagSet { return newValidateFlagSet(&validateOptions{}) },
			run:        validateCmd,
		},
		{
			name:       "doctor",
			summary:    "Check environment connectivity and readiness",
			newFlagSet: func() *flag.FlagSet { return newDoctorFlagSet(&doctorOptions{}) },
			run:        doctorCmd,
		},
		{
			name:       "reset",
			summary:    "Reset all simulation targets to a clean state",
			newFlagSet: func() *flag.FlagSet { return newResetFlagSet(&resetOptions{}) },
			run:        resetCmd,
		},
		{
			name:       "logs",
			summary:    "Show run bundle information",
			newFlagSet: func() *flag.FlagSet { return newLogsFlagSet(&logsOptions{}) },
			run:        logsCmd,
		},
		{
			name:       "forward",
			summary:    "Start kubectl port-forward for configured services",
			newFlagSet: func() *flag.FlagSet { return newForwardFlagSet(&forwardOptions{}) },
			run:        forwardCmd,
		},
		{
			name:       "version",
			summary:    "Print version information",
			newFlagSet: func() *flag.FlagSet { return newVersionFlagSet(&versionOptions{}) },
			run:        versionCmd,
		},
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(runner.ExitInvalidInput)
	}

	name := os.Args[1]
	for _, sub := range subcommands() {
		if sub.name == name {
			os.Exit(sub.run(os.Args[2:]))
		}
	}

	fmt.Fprintf(os.Stderr, "unknown command: %s\n", name)
	printUsage()
	os.Exit(runner.ExitInvalidInput)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: citadel <command> [flags] [args]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	for _, sub := range subcommands() {
		fmt.Fprintf(os.Stderr, "  %-9s %s\n", sub.name, sub.summary)
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Global flags, valid on every command:")
	fmt.Fprintln(os.Stderr, "  --config <path>      Path to the configuration file")
	fmt.Fprintln(os.Stderr, "  --output human|json  Output format")
	fmt.Fprintln(os.Stderr, "  --verbose            Write step-level detail to stderr")
	fmt.Fprintln(os.Stderr, "  --no-color           Disable color output")
}

// globalOptions holds the four flags that SDD section 4.1 declares valid on
// every command.
type globalOptions struct {
	configPath string
	output     string
	verbose    bool
	noColor    bool
}

// newFlagSet returns a flag set named name with the four global flags bound to
// opts. Every subcommand builds its flag set here, so none of them omits a flag.
func newFlagSet(name string, opts *globalOptions) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&opts.configPath, "config", configDefault(), "Path to the configuration file")
	fs.StringVar(&opts.output, "output", defaultOutput(), "Output format: human or json")
	fs.BoolVar(&opts.verbose, "verbose", false, "Write step-level detail to stderr")
	fs.BoolVar(&opts.noColor, "no-color", defaultNoColor(), "Disable color output")
	return fs
}

// apply validates the global options and puts them into effect.
func (o *globalOptions) apply() error {
	if o.output != outputHuman && o.output != outputJSON {
		return fmt.Errorf("unknown --output %q: want %s or %s", o.output, outputHuman, outputJSON)
	}
	if o.noColor {
		if err := os.Setenv("NO_COLOR", "1"); err != nil {
			return fmt.Errorf("set NO_COLOR: %w", err)
		}
	}

	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	return nil
}

// jsonOutput reports whether the command must print JSON to stdout.
func (o *globalOptions) jsonOutput() bool { return o.output == outputJSON }

// verbosef writes step-level detail to stderr when --verbose is present.
func (o *globalOptions) verbosef(format string, args ...any) {
	if !o.verbose {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

// parseGlobals parses args into fs and puts the global options into effect.
// parseGlobals returns the positional arguments and an exit code.
func parseGlobals(fs *flag.FlagSet, opts *globalOptions, args []string) ([]string, int) {
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "parse flags: %v\n", err)
		return nil, runner.ExitInvalidInput
	}
	if err := opts.apply(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return nil, runner.ExitInvalidInput
	}
	return fs.Args(), runner.ExitSuccess
}

// stdoutIsTTY reports whether stdout is a terminal.
func stdoutIsTTY() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// defaultOutput returns the --output default for the current stdout.
func defaultOutput() string { return defaultOutputFor(stdoutIsTTY()) }

// defaultOutputFor returns human for a TTY and json for anything else, which is
// the default of SDD section 4.1.
func defaultOutputFor(isTTY bool) string {
	if isTTY {
		return outputHuman
	}
	return outputJSON
}

// defaultNoColor returns the --no-color default for the current stdout.
func defaultNoColor() bool { return defaultNoColorFor(stdoutIsTTY(), os.Getenv("NO_COLOR")) }

// defaultNoColorFor reports whether color is off. SDD sections 4.1 and 4.5 make
// NO_COLOR and a non-TTY stdout imply --no-color.
func defaultNoColorFor(isTTY bool, noColorEnv string) bool {
	if noColorEnv != "" {
		return true
	}
	return !isTTY
}

// configDefault returns the configuration path to use without an explicit
// --config. SDD section 4.1 prefers ./citadel.yaml and falls back to
// $CITADEL_CONFIG.
func configDefault() string {
	if _, err := os.Stat(defaultConfigName); err == nil {
		return defaultConfigName
	}
	if v := os.Getenv("CITADEL_CONFIG"); v != "" {
		return v
	}
	return defaultConfigName
}

// writeJSONLine encodes v as one JSON object on a line of its own, which is the
// newline-delimited form of SDD section 4.6.
func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal output: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// emitJSONLine writes one JSON object to stdout and reports a write failure on
// stderr, because an output failure must not change an exit code.
func emitJSONLine(v any) {
	if err := writeJSONLine(os.Stdout, v); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
	}
}

type setVarsFlag struct {
	values map[string]string
}

func (s *setVarsFlag) String() string { return "" }

func (s *setVarsFlag) Set(val string) error {
	parts := strings.SplitN(val, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("set flag must be key=value, got %q", val)
	}
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[parts[0]] = parts[1]
	return nil
}

// runOptions holds the flag values of citadel run.
type runOptions struct {
	global      globalOptions
	junitPath   string
	timeout     string
	keepGoing   bool
	stepThrough bool
	bundleDir   string
	setVars     setVarsFlag
}

// newRunFlagSet returns the flag set of citadel run bound to o.
func newRunFlagSet(o *runOptions) *flag.FlagSet {
	fs := newFlagSet("run", &o.global)
	fs.StringVar(&o.junitPath, "junit", "", "Path to write JUnit XML report")
	fs.StringVar(&o.timeout, "timeout", "10m", "Whole-run ceiling, distinct from per-step timeouts")
	fs.BoolVar(&o.keepGoing, "keep-going", false, "Continue to the next scenario on failure")
	fs.BoolVar(&o.stepThrough, "step-through", false, "Pause before each step and wait for confirmation")
	fs.StringVar(&o.bundleDir, "bundle-dir", defaultBundleDir, "Directory for run bundle output")
	fs.Var(&o.setVars, "set", "Set a variable as key=value (repeatable)")
	return fs
}

func runCmd(args []string) int {
	var o runOptions
	paths, code := parseGlobals(newRunFlagSet(&o), &o.global, args)
	if code != runner.ExitSuccess {
		return code
	}
	return executeRun(&o, paths)
}

func executeRun(o *runOptions, paths []string) int {
	if o.stepThrough {
		info, err := os.Stdin.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice == 0 {
			fmt.Fprintln(os.Stderr, "--step-through requires an interactive terminal on stdin")
			return runner.ExitInvalidInput
		}
	}

	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "citadel run requires at least one scenario path")
		return runner.ExitInvalidInput
	}

	o.global.verbosef("load config %s\n", o.global.configPath)
	cfg, err := config.Load(o.global.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return runner.ExitInvalidInput
	}

	var timeout time.Duration
	if o.timeout != "" {
		timeout, err = time.ParseDuration(o.timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse timeout: %v\n", err)
			return runner.ExitInvalidInput
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rc := runner.RunConfig{
		ScenarioPaths: paths,
		Config:        cfg,
		Output:        o.global.output,
		JUnitPath:     o.junitPath,
		Verbose:       o.global.verbose,
		Timeout:       timeout,
		KeepGoing:     o.keepGoing,
		SetVars:       o.setVars.values,
		BundleDir:     o.bundleDir,
		StepThrough:   o.stepThrough,
	}

	return runner.Run(ctx, rc)
}

// validateOptions holds the flag values of citadel validate.
type validateOptions struct {
	global globalOptions
}

// newValidateFlagSet returns the flag set of citadel validate bound to o.
func newValidateFlagSet(o *validateOptions) *flag.FlagSet {
	return newFlagSet("validate", &o.global)
}

func validateCmd(args []string) int {
	var o validateOptions
	paths, code := parseGlobals(newValidateFlagSet(&o), &o.global, args)
	if code != runner.ExitSuccess {
		return code
	}
	return executeValidate(&o, paths)
}

// validateEvent is one machine-readable validate result of SDD section 4.6.
type validateEvent struct {
	Type    string   `json:"type"`
	Path    string   `json:"path,omitempty"`
	Outcome string   `json:"outcome"`
	Errors  []string `json:"errors,omitempty"`
}

func executeValidate(o *validateOptions, paths []string) int {
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "citadel validate requires at least one scenario path")
		return runner.ExitInvalidInput
	}

	reportFailure := func(path string, messages []string) {
		if o.global.jsonOutput() {
			emitJSONLine(validateEvent{Type: "validate.result", Path: path, Outcome: "fail", Errors: messages})
			return
		}
		for _, m := range messages {
			if path == "" {
				fmt.Fprintf(os.Stderr, "FAIL config: %s\n", m)
				continue
			}
			fmt.Fprintf(os.Stderr, "FAIL %s: %s\n", path, m)
		}
	}

	var cfg *config.CitadelConfig
	if _, statErr := os.Stat(o.global.configPath); statErr == nil {
		o.global.verbosef("load config %s\n", o.global.configPath)
		loaded, loadErr := config.Load(o.global.configPath)
		if loadErr != nil {
			reportFailure("", []string{loadErr.Error()})
			return runner.ExitInvalidInput
		}
		cfg = loaded

		secretErrs := scenario.ValidateConfigSecrets(cfg)
		if len(secretErrs) > 0 {
			reportFailure("", errorStrings(secretErrs))
			return runner.ExitInvalidInput
		}
	}

	expanded, expandErr := scenario.ExpandPaths(paths)
	if expandErr != nil {
		reportFailure("", []string{expandErr.Error()})
		return runner.ExitInvalidInput
	}

	hasError := false
	for _, path := range expanded {
		o.global.verbosef("validate %s\n", path)

		sc, err := scenario.Parse(path)
		if err != nil {
			reportFailure(path, []string{err.Error()})
			hasError = true
			continue
		}

		errs := scenario.ValidateWithOptions(sc, scenario.Options{BasePath: filepath.Dir(path), Config: cfg})
		if len(errs) > 0 {
			reportFailure(path, errorStrings(errs))
			hasError = true
			continue
		}

		if o.global.jsonOutput() {
			emitJSONLine(validateEvent{Type: "validate.result", Path: path, Outcome: "pass"})
			continue
		}
		fmt.Printf("OK   %s\n", path)
	}

	if hasError {
		if o.global.jsonOutput() {
			emitJSONLine(validateEvent{Type: "validate.end", Outcome: "fail"})
		}
		return runner.ExitInvalidInput
	}
	if o.global.jsonOutput() {
		emitJSONLine(validateEvent{Type: "validate.end", Outcome: "pass"})
	}
	return runner.ExitSuccess
}

// errorStrings converts a slice of errors into the message strings that both
// output formats need.
func errorStrings(errs []error) []string {
	messages := make([]string, len(errs))
	for i, e := range errs {
		messages[i] = e.Error()
	}
	return messages
}

// doctorOptions holds the flag values of citadel doctor.
type doctorOptions struct {
	global globalOptions
}

// newDoctorFlagSet returns the flag set of citadel doctor bound to o.
func newDoctorFlagSet(o *doctorOptions) *flag.FlagSet {
	return newFlagSet("doctor", &o.global)
}

func doctorCmd(args []string) int {
	var o doctorOptions
	if _, code := parseGlobals(newDoctorFlagSet(&o), &o.global, args); code != runner.ExitSuccess {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return executeDoctor(ctx, &o)
}

// doctorCheckEvent is one machine-readable doctor result of SDD section 4.6.
type doctorCheckEvent struct {
	Type string `json:"type"`
	diagnostics.CheckResult
}

// doctorEndEvent closes the machine-readable doctor stream.
type doctorEndEvent struct {
	Type     string `json:"type"`
	Outcome  string `json:"outcome"`
	ExitCode int    `json:"exitCode"`
}

func executeDoctor(ctx context.Context, o *doctorOptions) int {
	o.global.verbosef("load config %s\n", o.global.configPath)
	cfg, err := config.Load(o.global.configPath)
	if err != nil {
		// The config row of SDD section 12.3 is a check, so its failure
		// reports as one. SDD section 4.4 keeps a bad configuration at exit 2
		// and reserves exit 3 for an environment problem, so no further check
		// runs and the code stays 2.
		o.reportCheck(diagnostics.ConfigLoadFailure(err))
		return o.reportDoctorEnd(runner.ExitInvalidInput)
	}

	o.global.verbosef("run every doctor check\n")
	results := diagnostics.RunDoctor(ctx, cfg)

	allPass := true
	for _, r := range results {
		if r.Status == "fail" {
			allPass = false
		}
		o.reportCheck(r)
	}

	code := runner.ExitSuccess
	if !allPass {
		code = runner.ExitEnvironment
	}
	return o.reportDoctorEnd(code)
}

// reportCheck prints one doctor check in the format the output flag names.
func (o *doctorOptions) reportCheck(r diagnostics.CheckResult) {
	if o.global.jsonOutput() {
		emitJSONLine(doctorCheckEvent{Type: "doctor.check", CheckResult: r})
		return
	}
	fmt.Printf("[%s] %s: %s\n", strings.ToUpper(r.Status), r.Name, r.Message)
	if r.Remediation != "" {
		fmt.Printf("       fix: %s\n", r.Remediation)
	}
}

// reportDoctorEnd closes the machine-readable stream and returns the exit code
// unchanged.
func (o *doctorOptions) reportDoctorEnd(code int) int {
	if !o.global.jsonOutput() {
		return code
	}
	outcome := "pass"
	if code != runner.ExitSuccess {
		outcome = "fail"
	}
	emitJSONLine(doctorEndEvent{Type: "doctor.end", Outcome: outcome, ExitCode: code})
	return code
}

// resetOptions holds the flag values of citadel reset.
type resetOptions struct {
	global globalOptions
}

// newResetFlagSet returns the flag set of citadel reset bound to o.
func newResetFlagSet(o *resetOptions) *flag.FlagSet {
	return newFlagSet("reset", &o.global)
}

func resetCmd(args []string) int {
	var o resetOptions
	if _, code := parseGlobals(newResetFlagSet(&o), &o.global, args); code != runner.ExitSuccess {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return executeReset(ctx, &o)
}

// resetEvent is the machine-readable reset result of SDD section 4.6.
type resetEvent struct {
	Type     string `json:"type"`
	RunID    string `json:"runId"`
	Outcome  string `json:"outcome"`
	ExitCode int    `json:"exitCode"`
}

func executeReset(ctx context.Context, o *resetOptions) int {
	o.global.verbosef("load config %s\n", o.global.configPath)
	cfg, err := config.Load(o.global.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return runner.ExitInvalidInput
	}

	runID := runner.NewRunID()
	httpClient := &http.Client{Timeout: 30 * time.Second}

	o.global.verbosef("open every configured database (run_id=%s)\n", runID)
	databases, closeDatabases, err := runner.OpenDatabases(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database connection failed: %v\n", err)
		return o.reportReset(runID, runner.ClassifyError(err))
	}
	defer closeDatabases()

	o.global.verbosef("run the reset sequence\n")
	if err := runner.Reset(ctx, cfg, runID, httpClient, databases); err != nil {
		fmt.Fprintf(os.Stderr, "reset failed: %v\n", err)
		return o.reportReset(runID, runner.ClassifyError(err))
	}

	return o.reportReset(runID, runner.ExitSuccess)
}

// reportReset prints the reset outcome in the format the output flag names and
// returns the exit code unchanged.
func (o *resetOptions) reportReset(runID string, code int) int {
	outcome := "pass"
	if code != runner.ExitSuccess {
		outcome = "fail"
	}
	if o.global.jsonOutput() {
		emitJSONLine(resetEvent{Type: "reset.end", RunID: runID, Outcome: outcome, ExitCode: code})
		return code
	}
	if code == runner.ExitSuccess {
		fmt.Printf("reset complete (run_id=%s)\n", runID)
	}
	return code
}

// logsOptions holds the flag values of citadel logs.
type logsOptions struct {
	global      globalOptions
	lastFailure bool
	bundleDir   string
}

// newLogsFlagSet returns the flag set of citadel logs bound to o.
func newLogsFlagSet(o *logsOptions) *flag.FlagSet {
	fs := newFlagSet("logs", &o.global)
	fs.BoolVar(&o.lastFailure, "last-failure", false, "Show the most recent run bundle")
	fs.StringVar(&o.bundleDir, "bundle-dir", defaultBundleDir, "Directory that holds the run bundles")
	return fs
}

func logsCmd(args []string) int {
	var o logsOptions
	if _, code := parseGlobals(newLogsFlagSet(&o), &o.global, args); code != runner.ExitSuccess {
		return code
	}
	return executeLogs(&o)
}

// bundleSummary is the readable outcome of one run bundle that SDD section 12.1
// asks citadel logs to print.
type bundleSummary struct {
	Bundle     string `json:"bundle"`
	RunID      string `json:"runId,omitempty"`
	Scenario   string `json:"scenario,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	EndedAt    string `json:"endedAt,omitempty"`
	DurationMs *int64 `json:"durationMs,omitempty"`
}

// runFile mirrors the outcome and the timings of run.json. The runner owns the
// file, so an absent field leaves one line out of the summary.
type runFile struct {
	RunID      string `json:"runId"`
	Scenario   string `json:"scenario"`
	Outcome    string `json:"outcome"`
	ExitCode   *int   `json:"exitCode"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt"`
	DurationMs *int64 `json:"durationMs"`
	Timings    *struct {
		StartedAt  string `json:"startedAt"`
		EndedAt    string `json:"endedAt"`
		DurationMs *int64 `json:"durationMs"`
	} `json:"timings"`
}

func executeLogs(o *logsOptions) int {
	if !o.lastFailure {
		fmt.Fprintln(os.Stderr, "citadel logs requires --last-failure")
		return runner.ExitInvalidInput
	}

	o.global.verbosef("scan bundle directory %s\n", o.bundleDir)
	bundle, err := latestBundle(o.bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return runner.ExitSuccess
	}

	summary, raw := readBundleSummary(bundle)

	if o.global.jsonOutput() {
		emitJSONLine(summary)
		if len(raw) > 0 {
			o.global.verbosef("run.json:\n%s\n", raw)
		}
		return runner.ExitSuccess
	}

	writeBundleSummary(os.Stdout, summary)
	if o.global.verbose && len(raw) > 0 {
		fmt.Printf("run.json:\n%s\n", raw)
	}
	return runner.ExitSuccess
}

// latestBundle returns the newest run bundle directory under root.
func latestBundle(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("no run bundles found in %s", root)
	}

	newest := ""
	var newestTime time.Time
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Two bundles share a modification time when the filesystem clock is coarse.
	// The run ID is a UUIDv7 and sorts by time, so the last name of an equal pair
	// is the newer run.
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(root, name)
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if newest == "" || !info.ModTime().Before(newestTime) {
			newest = path
			newestTime = info.ModTime()
		}
	}

	if newest == "" {
		return "", fmt.Errorf("no run bundles found in %s", root)
	}
	return newest, nil
}

// readBundleSummary reads run.json from dir. readBundleSummary returns the
// summary and the raw file, and it tolerates a bundle without a readable
// run.json because the path alone is still useful.
func readBundleSummary(dir string) (bundleSummary, []byte) {
	summary := bundleSummary{Bundle: dir}

	raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		slog.Debug("bundle has no readable run.json", "bundle", dir, "error", err.Error())
		return summary, nil
	}

	var rf runFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		slog.Warn("run.json is not valid JSON", "bundle", dir, "error", err.Error())
		return summary, raw
	}

	summary.RunID = rf.RunID
	summary.Scenario = rf.Scenario
	summary.Outcome = rf.Outcome
	summary.ExitCode = rf.ExitCode
	summary.StartedAt = rf.StartedAt
	summary.EndedAt = rf.EndedAt
	summary.DurationMs = rf.DurationMs
	if rf.Timings != nil {
		if summary.StartedAt == "" {
			summary.StartedAt = rf.Timings.StartedAt
		}
		if summary.EndedAt == "" {
			summary.EndedAt = rf.Timings.EndedAt
		}
		if summary.DurationMs == nil {
			summary.DurationMs = rf.Timings.DurationMs
		}
	}
	if summary.RunID == "" {
		summary.RunID = filepath.Base(dir)
	}
	return summary, raw
}

// writeBundleSummary prints the readable form of one bundle summary.
func writeBundleSummary(w io.Writer, s bundleSummary) {
	fmt.Fprintf(w, "bundle:    %s\n", s.Bundle)
	if s.RunID != "" {
		fmt.Fprintf(w, "run id:    %s\n", s.RunID)
	}
	if s.Scenario != "" {
		fmt.Fprintf(w, "scenario:  %s\n", s.Scenario)
	}
	if s.Outcome != "" {
		fmt.Fprintf(w, "outcome:   %s\n", s.Outcome)
	}
	if s.ExitCode != nil {
		fmt.Fprintf(w, "exit code: %d\n", *s.ExitCode)
	}
	if s.StartedAt != "" {
		fmt.Fprintf(w, "started:   %s\n", s.StartedAt)
	}
	if s.EndedAt != "" {
		fmt.Fprintf(w, "ended:     %s\n", s.EndedAt)
	}
	if s.DurationMs != nil {
		fmt.Fprintf(w, "duration:  %s\n", (time.Duration(*s.DurationMs) * time.Millisecond).String())
	}
}

// forwardOptions holds the flag values of citadel forward.
type forwardOptions struct {
	global globalOptions
}

// newForwardFlagSet returns the flag set of citadel forward bound to o.
func newForwardFlagSet(o *forwardOptions) *flag.FlagSet {
	return newFlagSet("forward", &o.global)
}

func forwardCmd(args []string) int {
	var o forwardOptions
	if _, code := parseGlobals(newForwardFlagSet(&o), &o.global, args); code != runner.ExitSuccess {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return executeForward(ctx, &o)
}

// forwardEvent is the machine-readable form of one established forward.
type forwardEvent struct {
	Type       string `json:"type"`
	Service    string `json:"service"`
	LocalPort  int    `json:"localPort"`
	RemotePort int    `json:"remotePort"`
}

func executeForward(ctx context.Context, o *forwardOptions) int {
	o.global.verbosef("load config %s\n", o.global.configPath)
	cfg, err := config.Load(o.global.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return runner.ExitInvalidInput
	}

	if cfg.Kubernetes == nil || len(cfg.Kubernetes.Forwards) == 0 {
		fmt.Fprintln(os.Stderr, "no port-forward entries in kubernetes configuration")
		return runner.ExitInvalidInput
	}

	kubectlPath, lookErr := exec.LookPath("kubectl")
	if lookErr != nil {
		fmt.Fprintln(os.Stderr, "kubectl is not found in PATH")
		return runner.ExitEnvironment
	}

	// SDD section 4.7 rejects a half-established set of forwards, so every
	// started process goes down together with the first one that fails.
	var started []*exec.Cmd
	teardown := func() {
		for _, cmd := range started {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
	}

	for _, fwd := range cfg.Kubernetes.Forwards {
		cmdArgs := []string{
			"port-forward",
			fmt.Sprintf("svc/%s", fwd.Service),
			fmt.Sprintf("%d:%d", fwd.LocalPort, fwd.RemotePort),
		}
		if cfg.Kubernetes.Namespace != "" {
			cmdArgs = append(cmdArgs, "-n", cfg.Kubernetes.Namespace)
		}
		if cfg.Kubernetes.Context != "" {
			cmdArgs = append(cmdArgs, "--context", cfg.Kubernetes.Context)
		}

		// The kubectl narration is human output, so it goes to stderr and leaves
		// stdout to the mapping alone.
		cmd := exec.CommandContext(ctx, kubectlPath, cmdArgs...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if startErr := cmd.Start(); startErr != nil {
			fmt.Fprintf(os.Stderr, "start port-forward for %s: %v\n", fwd.Service, startErr)
			teardown()
			return runner.ExitEnvironment
		}
		started = append(started, cmd)

		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(fwd.LocalPort))
		o.global.verbosef("wait for %s on %s\n", fwd.Service, addr)
		if waitErr := waitForPort(ctx, addr, forwardReadyTimeout); waitErr != nil {
			fmt.Fprintf(os.Stderr, "service %s does not accept a connection on local port %d: %v\n",
				fwd.Service, fwd.LocalPort, waitErr)
			teardown()
			return runner.ExitEnvironment
		}

		if o.global.jsonOutput() {
			emitJSONLine(forwardEvent{
				Type:       "forward.ready",
				Service:    fwd.Service,
				LocalPort:  fwd.LocalPort,
				RemotePort: fwd.RemotePort,
			})
			continue
		}
		fmt.Printf("forwarding %s localhost:%d -> %d\n", fwd.Service, fwd.LocalPort, fwd.RemotePort)
	}

	<-ctx.Done()
	teardown()
	return runner.ExitSuccess
}

// waitForPort dials addr until it accepts a connection. waitForPort returns an
// error when the timeout expires or the context ends first.
func waitForPort(ctx context.Context, addr string, timeout time.Duration) error {
	const interval = 25 * time.Millisecond

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		dialer := net.Dialer{Timeout: timeout}
		conn, err := dialer.DialContext(waitCtx, "tcp", addr)
		if err == nil {
			if closeErr := conn.Close(); closeErr != nil {
				return fmt.Errorf("close the probe connection to %s: %w", addr, closeErr)
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("wait for %s: %w", addr, ctxErr)
		}

		select {
		case <-waitCtx.Done():
			// waitCtx ends when the caller's context ends as well as on the
			// timeout, and the two need different messages. A cancelled run
			// never waited the full window, and reporting that it did sends the
			// reader to look for a service that is slow to start.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("wait for %s: %w", addr, ctxErr)
			}
			return fmt.Errorf("wait for %s: no connection within %s: %w", addr, timeout, err)
		case <-time.After(interval):
		}
	}
}

// versionOptions holds the flag values of citadel version.
type versionOptions struct {
	global globalOptions
}

// newVersionFlagSet returns the flag set of citadel version bound to o.
func newVersionFlagSet(o *versionOptions) *flag.FlagSet {
	return newFlagSet("version", &o.global)
}

func versionCmd(args []string) int {
	var o versionOptions
	if _, code := parseGlobals(newVersionFlagSet(&o), &o.global, args); code != runner.ExitSuccess {
		return code
	}

	if err := writeVersion(os.Stdout, currentBuildInfo(), o.global.output); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return runner.ExitInternal
	}
	return runner.ExitSuccess
}

// buildInfo describes the build identity that citadel version prints.
type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}

// currentBuildInfo returns the build identity of this binary.
func currentBuildInfo() buildInfo {
	return buildInfo{Version: version, Commit: commit, BuildDate: buildDate}
}

// writeVersion prints the build identity in the format that output names.
func writeVersion(w io.Writer, info buildInfo, output string) error {
	if output == outputJSON {
		return writeJSONLine(w, info)
	}
	if _, err := fmt.Fprintf(w, "citadel %s\ncommit %s\nbuilt %s\n", info.Version, info.Commit, info.BuildDate); err != nil {
		return fmt.Errorf("write version: %w", err)
	}
	return nil
}
