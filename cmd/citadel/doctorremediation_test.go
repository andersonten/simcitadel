package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/runner"
)

// configWithAnUnsetReference writes a configuration file that references an
// environment variable which no test sets, and returns its path with the name
// of that variable.
func configWithAnUnsetReference(t *testing.T) (path, variable string) {
	t.Helper()

	variable = "CITADEL_DOCTOR_ABSENT_URL"
	if _, present := os.LookupEnv(variable); present {
		t.Fatalf("the test needs %s unset", variable)
	}

	path = filepath.Join(t.TempDir(), "citadel.yaml")
	body := "version: 1\n" +
		"environment:\n" +
		"  name: local\n" +
		"targets:\n" +
		"  event-trigger:\n" +
		"    url: \"${" + variable + "}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the configuration file: %v", err)
	}
	return path, variable
}

// TestExecuteDoctor_UnsetVariable_ReportsTheConfigCheckWithARemediation holds
// the first row of the SDD section 12.3 table. That row is a doctor check, and
// section 12.3 states that every check reports pass, fail or skip "with a
// remediation sentence on failure", so the one failure the row exists for must
// reach the check stream rather than bypass it. Section 4.6 puts the
// machine-readable result on stdout, and section 5.2 makes doctor the command
// that "tells them exactly which are missing".
func TestExecuteDoctor_UnsetVariable_ReportsTheConfigCheckWithARemediation(t *testing.T) {
	path, variable := configWithAnUnsetReference(t)

	var code int
	out := captureStdout(t, func() {
		o := doctorOptions{}
		o.global.configPath = path
		o.global.output = outputJSON
		code = executeDoctor(context.Background(), &o)
	})

	// SDD section 4.4 gives bad configuration exit 2 and keeps 3 for an
	// environment problem, so the exit code stays 2.
	if code != runner.ExitInvalidInput {
		t.Errorf("exit code = %d, want %d", code, runner.ExitInvalidInput)
	}

	type event struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Status      string `json:"status"`
		Message     string `json:"message"`
		Remediation string `json:"remediation"`
		ExitCode    int    `json:"exitCode"`
		Outcome     string `json:"outcome"`
	}

	var checks, ends []event
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("stdout line %q is not JSON: %v", line, err)
		}
		switch e.Type {
		case "doctor.check":
			checks = append(checks, e)
		case "doctor.end":
			ends = append(ends, e)
		}
	}

	if len(checks) == 0 {
		t.Fatalf("doctor emitted no check on stdout. stdout was %q", out)
	}
	got := checks[0]
	if got.Status != "fail" {
		t.Errorf("the config check reports %q, want fail", got.Status)
	}
	if !strings.Contains(got.Message, variable) {
		t.Errorf("the message %q does not name the unset variable", got.Message)
	}
	if strings.TrimSpace(got.Remediation) == "" {
		t.Error("the config check failure carries no remediation sentence")
	}
	if len(ends) != 1 {
		t.Fatalf("doctor emitted %d doctor.end events, want 1", len(ends))
	}
	if ends[0].ExitCode != runner.ExitInvalidInput {
		t.Errorf("doctor.end reports exitCode %d, want %d", ends[0].ExitCode, runner.ExitInvalidInput)
	}
}

// TestExecuteDoctor_UnsetVariableInHumanOutput_PrintsTheRemediation covers the
// other half of the same row. The human form is what a developer reads, and
// section 12.3 puts the whole value of doctor in the remediation text.
func TestExecuteDoctor_UnsetVariableInHumanOutput_PrintsTheRemediation(t *testing.T) {
	path, variable := configWithAnUnsetReference(t)

	out := captureStdout(t, func() {
		o := doctorOptions{}
		o.global.configPath = path
		o.global.output = outputHuman
		executeDoctor(context.Background(), &o)
	})

	if !strings.Contains(out, variable) {
		t.Errorf("stdout %q does not name the unset variable", out)
	}
	if !strings.Contains(out, "fix:") {
		t.Errorf("stdout %q carries no remediation line", out)
	}
}
