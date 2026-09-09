package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/runner"
)

// fakeKubectl puts a kubectl on PATH that starts and then does nothing. The
// real command opens the local port, so a stand-in that never opens it
// separates the readiness wait of SDD section 4.7 from a bare process start.
func fakeKubectl(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	// exec replaces the shell, so the teardown kill reaches the sleep itself.
	// A forked child would outlive the kill and hold the inherited pipes open.
	script := "#!/bin/sh\nexec sleep 60\n"
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake kubectl: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// forwardConfig writes a configuration with one port-forward entry and returns
// its path.
func forwardConfig(t *testing.T, localPort int) string {
	t.Helper()

	content := "version: 1\n" +
		"environment:\n  name: local\n" +
		"kubernetes:\n" +
		"  namespace: default\n" +
		"  forwards:\n" +
		"    - service: citadel-wiremock\n" +
		"      localPort: " + strconv.Itoa(localPort) + "\n" +
		"      remotePort: 8080\n"

	path := filepath.Join(t.TempDir(), "citadel.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the forward config: %v", err)
	}
	return path
}

// freePort returns a local port that no process listens on.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

// TestExecuteForward_LocalPortNeverAccepts_ReportsAnEnvironmentFailure holds
// SDD section 4.7, which waits for each local port to accept a connection
// before it prints the mapping. A forward that only starts kubectl announces a
// mapping that carries no traffic, and the run then fails somewhere else.
func TestExecuteForward_LocalPortNeverAccepts_ReportsAnEnvironmentFailure(t *testing.T) {
	fakeKubectl(t)
	port := freePort(t)

	o := &forwardOptions{global: globalOptions{
		configPath: forwardConfig(t, port),
		output:     "human",
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	var code int
	out := captureStdout(t, func() { code = executeForward(ctx, o) })

	if code != runner.ExitEnvironment {
		t.Errorf("exit = %d, want %d, because the local port never accepts a connection",
			code, runner.ExitEnvironment)
	}
	if strings.Contains(out, "forwarding") {
		t.Errorf("forward announced a mapping for a port that accepts nothing: %q", out)
	}
}

// TestExecuteForward_LocalPortAccepts_AnnouncesTheMapping proves the wait ends
// as soon as the port answers, so a working forward still reports itself.
func TestExecuteForward_LocalPortAccepts_AnnouncesTheMapping(t *testing.T) {
	fakeKubectl(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	o := &forwardOptions{global: globalOptions{
		configPath: forwardConfig(t, port),
		output:     "human",
	}}

	// The bound is a backstop against a hang. The announcement below ends the
	// command in the ordinary case, well inside it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The readiness probe dials the listener once. Accepting in a loop keeps the
	// port answering for as long as the command runs.
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	// The command runs until the context ends, so the test ends it. The
	// condition is the announcement itself, and never the accepted connection:
	// a cancel at the moment the port answers races the probe that is dialling
	// it, and the command then reports a cancelled context as an environment
	// failure. Waiting for the announcement keeps the test off that race and off
	// a fixed sleep.
	out, announced := captureStdoutUntil(t, "forwarding citadel-wiremock")

	var code int
	done := make(chan struct{})
	go func() {
		defer close(done)
		code = executeForward(ctx, o)
	}()

	select {
	case <-announced:
	case <-ctx.Done():
	}
	cancel()
	<-done

	printed := out()

	if code != runner.ExitSuccess {
		t.Errorf("exit = %d, want %d for a forward whose port answers", code, runner.ExitSuccess)
	}
	if !strings.Contains(printed, "forwarding citadel-wiremock") {
		t.Errorf("forward did not announce the mapping: %q", printed)
	}
}

// captureStdoutUntil redirects stdout and closes the returned channel as soon
// as stdout holds want. The returned function restores stdout and answers with
// everything written. It exists because captureStdout cannot report progress:
// it reads the pipe to the end, and a command that runs until its context ends
// gives the test no other condition to wait for.
func captureStdoutUntil(t *testing.T, want string) (func() string, <-chan struct{}) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = w

	seen := make(chan struct{})
	collected := make(chan string, 1)

	go func() {
		var out strings.Builder
		buf := make([]byte, 512)
		found := false
		for {
			n, readErr := r.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
				if !found && strings.Contains(out.String(), want) {
					found = true
					close(seen)
				}
			}
			if readErr != nil {
				if !found {
					close(seen)
				}
				collected <- out.String()
				return
			}
		}
	}()

	return func() string {
		os.Stdout = original
		if closeErr := w.Close(); closeErr != nil {
			t.Fatalf("close pipe writer: %v", closeErr)
		}
		out := <-collected
		if closeErr := r.Close(); closeErr != nil {
			t.Fatalf("close pipe reader: %v", closeErr)
		}
		return out
	}, seen
}
