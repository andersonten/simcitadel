package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStdout replaces the process stdout with a pipe, runs fn, and returns
// what fn wrote. The commands print a result to stdout, so a test reads it here.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()

	fn()

	os.Stdout = original
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return out
}

// writeBundle creates one run bundle directory with the given run.json body and
// sets its modification time, so a test controls which bundle is the newest.
func writeBundle(t *testing.T, root, runID, body string, modTime time.Time) string {
	t.Helper()

	dir := filepath.Join(root, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create bundle directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write run.json: %v", err)
	}
	if err := os.Chtimes(dir, modTime, modTime); err != nil {
		t.Fatalf("set bundle modification time: %v", err)
	}
	return dir
}

// TestConfigDefault_LocalFileAndEnvironmentVariable_PrefersTheLocalFile locks
// down the resolution order of SDD section 4.1: ./citadel.yaml, then
// $CITADEL_CONFIG.
func TestConfigDefault_LocalFileAndEnvironmentVariable_PrefersTheLocalFile(t *testing.T) {
	cases := []struct {
		name        string
		writeLocal  bool
		environment string
		want        string
	}{
		{"local file wins over the environment variable", true, "/from/env.yaml", "citadel.yaml"},
		{"environment variable applies without a local file", false, "/from/env.yaml", "/from/env.yaml"},
		{"neither present falls back to the local name", false, "", "citadel.yaml"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("CITADEL_CONFIG", tc.environment)
			if tc.writeLocal {
				if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte("environment: local\n"), 0o600); err != nil {
					t.Fatalf("write local config: %v", err)
				}
			}

			if got := configDefault(); got != tc.want {
				t.Errorf("configDefault() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGlobalFlags_ExplicitConfig_WinsOverBothDefaults keeps an explicit
// --config ahead of the local file and of $CITADEL_CONFIG.
func TestGlobalFlags_ExplicitConfig_WinsOverBothDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CITADEL_CONFIG", "/from/env.yaml")
	if err := os.WriteFile(filepath.Join(dir, "citadel.yaml"), []byte("environment: local\n"), 0o600); err != nil {
		t.Fatalf("write local config: %v", err)
	}

	var opts globalOptions
	fs := newFlagSet("logs", &opts)
	fs.SetOutput(io.Discard)
	if err := fs.Parse([]string{"--config", "explicit.yaml"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	if opts.configPath != "explicit.yaml" {
		t.Errorf("config path = %q, want %q", opts.configPath, "explicit.yaml")
	}
}

// TestSubcommands_EveryFlagSet_DefinesTheGlobalFlags is the gate of SDD section
// 4.1. A new subcommand that forgets one of the four flags fails here.
func TestSubcommands_EveryFlagSet_DefinesTheGlobalFlags(t *testing.T) {
	global := []string{"config", "output", "verbose", "no-color"}

	for _, sub := range subcommands() {
		t.Run(sub.name, func(t *testing.T) {
			fs := sub.newFlagSet()
			for _, name := range global {
				if fs.Lookup(name) == nil {
					t.Errorf("subcommand %s does not define --%s", sub.name, name)
				}
			}
		})
	}
}

// TestSubcommands_OutputFlagDefault_MatchesTheSharedDefault keeps the TTY rule
// of SDD section 4.1 in one place for every subcommand.
func TestSubcommands_OutputFlagDefault_MatchesTheSharedDefault(t *testing.T) {
	want := defaultOutput()

	for _, sub := range subcommands() {
		t.Run(sub.name, func(t *testing.T) {
			f := sub.newFlagSet().Lookup("output")
			if f == nil {
				t.Fatalf("subcommand %s does not define --output", sub.name)
			}
			if f.DefValue != want {
				t.Errorf("subcommand %s --output default = %q, want %q", sub.name, f.DefValue, want)
			}
		})
	}
}

// TestDefaultOutputFor_TTYAndPipe_SelectsHumanThenJSON locks down the default
// of SDD section 4.1.
func TestDefaultOutputFor_TTYAndPipe_SelectsHumanThenJSON(t *testing.T) {
	if got := defaultOutputFor(true); got != "human" {
		t.Errorf("defaultOutputFor(true) = %q, want human", got)
	}
	if got := defaultOutputFor(false); got != "json" {
		t.Errorf("defaultOutputFor(false) = %q, want json", got)
	}
}

// TestDefaultNoColorFor_EnvironmentOrPipe_ImpliesNoColor holds the implication
// of SDD sections 4.1 and 4.5.
func TestDefaultNoColorFor_EnvironmentOrPipe_ImpliesNoColor(t *testing.T) {
	cases := []struct {
		name        string
		isTTY       bool
		environment string
		want        bool
	}{
		{"a TTY without NO_COLOR keeps color", true, "", false},
		{"NO_COLOR suppresses color on a TTY", true, "1", true},
		{"a non-TTY stdout suppresses color", false, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultNoColorFor(tc.isTTY, tc.environment); got != tc.want {
				t.Errorf("defaultNoColorFor(%v, %q) = %v, want %v", tc.isTTY, tc.environment, got, tc.want)
			}
		})
	}
}

// TestGlobalOptions_NoColor_SetsTheNoColorEnvironmentVariable proves the flag is
// honoured and not only accepted.
func TestGlobalOptions_NoColor_SetsTheNoColorEnvironmentVariable(t *testing.T) {
	t.Setenv("NO_COLOR", "")

	opts := globalOptions{output: "json", noColor: true}
	if err := opts.apply(); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if os.Getenv("NO_COLOR") == "" {
		t.Error("NO_COLOR is empty after --no-color")
	}
}

// TestGlobalOptions_UnknownOutputFormat_IsInvalidInput keeps an unusable value
// out of the commands that print a result.
func TestGlobalOptions_UnknownOutputFormat_IsInvalidInput(t *testing.T) {
	opts := globalOptions{output: "yaml"}
	if err := opts.apply(); err == nil {
		t.Error("apply accepted --output yaml")
	}
}

// TestWaitForPort_ListenerAccepts_ReturnsNoError proves the readiness wait of
// SDD section 4.7 detects an established forward.
func TestWaitForPort_ListenerAccepts_ReturnsNoError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	if err := waitForPort(context.Background(), listener.Addr().String(), 5*time.Second); err != nil {
		t.Errorf("waitForPort on an open port = %v, want nil", err)
	}
}

// TestWaitForPort_NoListener_ReturnsATimeoutError proves a forward that never
// comes up is reported rather than announced as established.
func TestWaitForPort_NoListener_ReturnsATimeoutError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	err = waitForPort(context.Background(), addr, 200*time.Millisecond)
	if err == nil {
		t.Fatal("waitForPort on a closed port = nil, want an error")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error %q does not name the address %q", err, addr)
	}
}

// TestVersionCmd_WithoutATTY_DefaultsToJSONOutput holds the --output default of
// SDD section 4.1 on every command, not on run alone.
func TestVersionCmd_WithoutATTY_DefaultsToJSONOutput(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code = versionCmd(nil) })

	if code != 0 {
		t.Fatalf("versionCmd exit code = %d, want 0", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("output %q is not JSON: %v", out, err)
	}
}

// TestVersionCmd_JSONOutput_CarriesVersionCommitAndBuildDate holds the contract
// of SDD section 4.1 for citadel version.
func TestVersionCmd_JSONOutput_CarriesVersionCommitAndBuildDate(t *testing.T) {
	var code int
	out := captureStdout(t, func() { code = versionCmd([]string{"--output", "json"}) })

	if code != 0 {
		t.Fatalf("versionCmd exit code = %d, want 0", code)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("output %q is not JSON: %v", out, err)
	}
	for _, key := range []string{"version", "commit", "buildDate"} {
		if got[key] == "" {
			t.Errorf("JSON output has no non-empty %q key, got %v", key, got)
		}
	}
}

// TestVersionCmd_GlobalFlags_AreAccepted proves the four flags of SDD section
// 4.1 are valid on version.
func TestVersionCmd_GlobalFlags_AreAccepted(t *testing.T) {
	var code int
	captureStdout(t, func() {
		code = versionCmd([]string{"--config", "citadel.yaml", "--verbose", "--no-color", "--output", "json"})
	})

	if code != 0 {
		t.Fatalf("versionCmd exit code = %d, want 0", code)
	}
}

// TestLogsCmd_GlobalFlags_AreAccepted proves the four flags of SDD section 4.1
// are valid on logs, which had none of them.
func TestLogsCmd_GlobalFlags_AreAccepted(t *testing.T) {
	empty := t.TempDir()
	var code int
	captureStdout(t, func() {
		code = logsCmd([]string{
			"--last-failure",
			"--bundle-dir", empty,
			"--config", "citadel.yaml",
			"--output", "json",
			"--verbose",
			"--no-color",
		})
	})

	if code != 0 {
		t.Fatalf("logsCmd exit code = %d, want 0", code)
	}
}

// TestLogsCmd_LastFailure_ChoosesTheNewestBundleAndNamesTheOutcome holds the
// summary contract of SDD section 12.1 and keeps logs on the bundle directory
// the user names.
func TestLogsCmd_LastFailure_ChoosesTheNewestBundleAndNamesTheOutcome(t *testing.T) {
	root := t.TempDir()
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Minute)
	writeBundle(t, root, "0191-older", `{"runId":"0191-older","outcome":"pass","exitCode":0}`, older)
	newest := writeBundle(t, root, "0191-newer", `{"runId":"0191-newer","scenario":"editorial-assignment","outcome":"fail","exitCode":1,"startedAt":"2026-08-19T09:00:00Z","endedAt":"2026-08-19T09:00:13Z","durationMs":13000,"config":{"environment":"local"}}`, newer)

	var code int
	out := captureStdout(t, func() {
		code = logsCmd([]string{"--last-failure", "--bundle-dir", root, "--output", "human"})
	})

	if code != 0 {
		t.Fatalf("logsCmd exit code = %d, want 0", code)
	}
	if !strings.Contains(out, newest) {
		t.Errorf("output does not name the newest bundle %q, got %q", newest, out)
	}
	if strings.Contains(out, "0191-older") {
		t.Errorf("output names the older bundle, got %q", out)
	}
	if !strings.Contains(out, "fail") {
		t.Errorf("output does not name the outcome, got %q", out)
	}
	if !strings.Contains(out, "1") {
		t.Errorf("output does not name the exit code, got %q", out)
	}
	if strings.Contains(out, "environment") {
		t.Errorf("output dumps the raw run.json without --verbose, got %q", out)
	}
}

// TestLogsCmd_LastFailureWithVerbose_KeepsTheRawRunFile holds the raw dump
// behind --verbose.
func TestLogsCmd_LastFailureWithVerbose_KeepsTheRawRunFile(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, "0191-only", `{"outcome":"fail","exitCode":1,"config":{"environment":"local"}}`, time.Now())

	var code int
	out := captureStdout(t, func() {
		code = logsCmd([]string{"--last-failure", "--bundle-dir", root, "--output", "human", "--verbose"})
	})

	if code != 0 {
		t.Fatalf("logsCmd exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "environment") {
		t.Errorf("verbose output omits the raw run.json, got %q", out)
	}
}

// TestLogsCmd_LastFailureWithJSONOutput_EmitsOneObject holds SDD section 4.6 on
// logs.
func TestLogsCmd_LastFailureWithJSONOutput_EmitsOneObject(t *testing.T) {
	root := t.TempDir()
	writeBundle(t, root, "0191-only", `{"outcome":"fail","exitCode":1}`, time.Now())

	var code int
	out := captureStdout(t, func() {
		code = logsCmd([]string{"--last-failure", "--bundle-dir", root, "--output", "json"})
	})

	if code != 0 {
		t.Fatalf("logsCmd exit code = %d, want 0", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("output %q is not JSON: %v", out, err)
	}
	if got["outcome"] != "fail" {
		t.Errorf("JSON output outcome = %v, want fail", got["outcome"])
	}
	if got["bundle"] == nil {
		t.Errorf("JSON output has no bundle path, got %v", got)
	}
}
