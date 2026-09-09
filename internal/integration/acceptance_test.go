//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The compose stack of SDD section 11.1 and the reference pack of SDD section
// 14.4, as paths relative to this package.
const (
	repoRoot           = "../.."
	composeDir         = repoRoot + "/deploy/compose"
	referenceConfig    = "examples/reference/citadel.yaml"
	referenceScenarios = "examples/reference/scenarios"
)

// referenceRunCount is the number of consecutive runs that SDD section 14.4
// asks for. The suite fails on any one of them, because a flake in this
// pipeline is a defect and not noise.
const referenceRunCount = 10

// The time limits of the external commands this file runs. A compose start
// builds three images on a cold cache, so it needs the largest one.
const (
	stackUpTimeout   = 15 * time.Minute
	stackDownTimeout = 5 * time.Minute
	buildTimeout     = 10 * time.Minute
	citadelTimeout   = 5 * time.Minute
)

// escape is the first byte of an ANSI colour sequence. SDD section 4.5 removes
// colour without a terminal, so this byte must not reach either stream.
const escape = "\x1b"

var (
	stackOnce    sync.Once
	stackErr     error
	stackStarted bool
)

// TestMain stops the compose stack that an acceptance test starts. The volume
// goes with it, because a surviving volume keeps the replication slot that SDD
// section 4.9 names as the one state a reset cannot undo.
func TestMain(m *testing.M) {
	code := m.Run()
	if stackStarted {
		if err := runCommand(context.Background(), composeDir, stackDownTimeout,
			"docker", "compose", "down", "-v", "--remove-orphans"); err != nil {
			fmt.Fprintf(os.Stderr, "stop the compose stack: %v\n", err)
		}
	}
	os.Exit(code)
}

// startComposeStack brings the default profile up once for this test binary and
// waits for every service to report healthy.
func startComposeStack(t *testing.T) {
	t.Helper()

	stackOnce.Do(func() {
		stackErr = runCommand(context.Background(), composeDir, stackUpTimeout,
			"docker", "compose", "up", "-d", "--wait", "--wait-timeout", "600")
		stackStarted = stackErr == nil
	})
	if stackErr != nil {
		t.Fatalf("bring the compose stack up: %v", stackErr)
	}
}

// buildCitadel builds the command line tool under test and returns its path.
// The acceptance tests drive the real binary, because the exit code and the
// output streams are the interface of SDD sections 4.4 and 4.6.
func buildCitadel(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "citadel")
	if err := runCommand(context.Background(), repoRoot, buildTimeout,
		"go", "build", "-o", binary, "./cmd/citadel"); err != nil {
		t.Fatalf("build the citadel binary: %v", err)
	}
	return binary
}

// hostEnv returns the variables that the reference configuration reads. Every
// address is a port that SDD section 11.1 publishes on loopback.
//
// managementUrl carries no credentials, which is the shape of the SDD section
// 5.1 sample: the user and the password belong to amqpUrl alone. The values
// below are the compose defaults and not a real credential.
func hostEnv() []string {
	return []string{
		"WIREMOCK_URL=http://127.0.0.1:9090",
		"WIREMOCK_ADMIN_URL=http://127.0.0.1:9090/__admin",
		"EVENT_TRIGGER_URL=http://127.0.0.1:8081",
		"EVENT_CAPTURE_URL=http://127.0.0.1:8082",
		"RABBITMQ_AMQP_URL=amqp://citadel:citadel@127.0.0.1:5672/",
		"RABBITMQ_MANAGEMENT_URL=http://127.0.0.1:15672",
		"APP_URL=http://127.0.0.1:5000",
		"APP_DB_DSN=postgres://citadel:citadel@127.0.0.1:5432/citadel?sslmode=disable",
	}
}

// runResult holds the outcome of one citadel invocation.
type runResult struct {
	exitCode int
	stdout   string
	stderr   string
}

// runCitadel runs the command line tool with stdin closed, with no terminal on
// either stream, and with TERM=dumb. Those are the conditions of constraint C2
// in SDD section 14.3, so every acceptance run holds them.
func runCitadel(t *testing.T, binary string, args ...string) runResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), citadelTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = repoRoot
	cmd.Env = append(hostEnv(), "TERM=dumb", "PATH="+os.Getenv("PATH"))
	// A nil Stdin gives the child an empty reader, so a command that waits for
	// input blocks forever instead of reading the test runner's terminal.
	cmd.Stdin = nil

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := runResult{stdout: stdout.String(), stderr: stderr.String()}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run citadel %v: %v\nstderr:\n%s", args, err, result.stderr)
	}
	return result
}

// runCommand runs one external command in dir. The error carries the combined
// output, because a compose failure or a build failure is unreadable without it.
func runCommand(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, output)
	}
	return nil
}

// TestReferenceScenario_TenConsecutiveRuns_AllPass is SDD section 14.4. The
// reference pack runs ten times against the compose stack and the suite fails
// on any flake, which makes repeatability a test rather than a claim.
func TestReferenceScenario_TenConsecutiveRuns_AllPass(t *testing.T) {
	startComposeStack(t)
	binary := buildCitadel(t)

	for attempt := 1; attempt <= referenceRunCount; attempt++ {
		result := runCitadel(t, binary, "run", "--config", referenceConfig, referenceScenarios)
		if result.exitCode != 0 {
			t.Fatalf("run %d of %d exited %d, want 0\nstdout:\n%s\nstderr:\n%s",
				attempt, referenceRunCount, result.exitCode, result.stdout, result.stderr)
		}
	}
}

// junitReport is the part of the JUnit XML that SDD section 4.6 specifies: one
// testcase per step and assertion, the step name as the test name, and the
// scenario name as the classname.
type junitReport struct {
	XMLName xml.Name `xml:"testsuites"`
	Suites  []struct {
		Name  string `xml:"name,attr"`
		Tests int    `xml:"tests,attr"`
		Cases []struct {
			Name      string `xml:"name,attr"`
			Classname string `xml:"classname,attr"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

// TestConstraintC2_NoTerminalAndClosedStdin_GivesJSONAndJUnitAndTheExitCode is
// constraint C2 of SDD section 14.3. The reference scenario runs with stdin
// closed, no terminal and TERM=dumb, and it must produce valid JSON output, a
// JUnit file that parses, and the documented exit code.
func TestConstraintC2_NoTerminalAndClosedStdin_GivesJSONAndJUnitAndTheExitCode(t *testing.T) {
	startComposeStack(t)
	binary := buildCitadel(t)

	junitPath := filepath.Join(t.TempDir(), "report.xml")
	// No --output flag. SDD section 4.1 makes json the default without a
	// terminal, and that default is half of what C2 asserts.
	result := runCitadel(t, binary, "run",
		"--config", referenceConfig, "--junit", junitPath, referenceScenarios)

	if result.exitCode != 0 {
		t.Fatalf("the reference run exited %d, want 0\nstdout:\n%s\nstderr:\n%s",
			result.exitCode, result.stdout, result.stderr)
	}

	// SDD section 4.6: one JSON object per line on stdout, and human narration
	// on stderr, so that a redirect of stdout alone is clean.
	lines := strings.Split(strings.TrimSpace(result.stdout), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("stdout carries no event line\nstderr:\n%s", result.stderr)
	}

	types := make(map[string]int, len(lines))
	for i, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("stdout line %d is not one JSON object: %v\nline: %s", i+1, err, line)
		}
		for _, key := range []string{"ts", "runId", "type"} {
			if _, ok := event[key]; !ok {
				t.Errorf("stdout line %d omits %q: %s", i+1, key, line)
			}
		}
		eventType, _ := event["type"].(string)
		types[eventType]++
	}

	// SDD section 4.2 runs the lifecycle once per run, so the stream opens and
	// closes exactly once.
	for _, eventType := range []string{"run.start", "run.end"} {
		if types[eventType] != 1 {
			t.Errorf("the stream carries %d %s events, want 1", types[eventType], eventType)
		}
	}

	// SDD section 4.5 removes colour without a terminal.
	if strings.Contains(result.stdout, escape) {
		t.Error("stdout carries an ANSI escape sequence without a terminal")
	}
	if strings.Contains(result.stderr, escape) {
		t.Error("stderr carries an ANSI escape sequence without a terminal")
	}

	raw, err := os.ReadFile(junitPath)
	if err != nil {
		t.Fatalf("read the JUnit report: %v", err)
	}

	var report junitReport
	if err := xml.Unmarshal(raw, &report); err != nil {
		t.Fatalf("the JUnit report does not parse: %v\n%s", err, raw)
	}
	if len(report.Suites) == 0 {
		t.Fatalf("the JUnit report holds no testsuite\n%s", raw)
	}

	for _, suite := range report.Suites {
		if suite.Name == "" {
			t.Error("a JUnit testsuite carries no name, and SDD section 4.6 names it for the scenario")
		}
		if len(suite.Cases) == 0 {
			t.Errorf("JUnit suite %q holds no testcase", suite.Name)
		}
		if suite.Tests != len(suite.Cases) {
			t.Errorf("JUnit suite %q reports tests=%d and holds %d testcase elements",
				suite.Name, suite.Tests, len(suite.Cases))
		}
		for _, testcase := range suite.Cases {
			if testcase.Name == "" {
				t.Errorf("a testcase of suite %q carries no name", suite.Name)
			}
			if testcase.Classname != suite.Name {
				t.Errorf("testcase %q carries classname %q, want the scenario name %q",
					testcase.Name, testcase.Classname, suite.Name)
			}
		}
	}
}
