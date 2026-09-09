package config

import (
	"os"
	"path/filepath"
	"testing"
)

// This file locks down the content directories of SDD section 5.1. These tests
// began as a proof that validate looked for a payload beside the scenario file
// while the runner looked for it under paths.payloads, so a scenario could pass
// validation and then fail to find the same file at run time.

// TestConfig_ContentPaths_ResolveAgainstTheConfigDirectory keeps a run
// independent of the working directory it starts from.
func TestConfig_ContentPaths_ResolveAgainstTheConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "citadel.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"payload", cfg.PayloadPath("article-submitted.v1.0.0.json"), filepath.Join(dir, "payloads", "article-submitted.v1.0.0.json")},
		{"sql", cfg.SQLPath("seed.sql"), filepath.Join(dir, "sql", "seed.sql")},
		{"query", cfg.QueryPath("pe-tasks.graphql"), filepath.Join(dir, "queries", "pe-tasks.graphql")},
		{"wiremock", cfg.WireMockPath("stubs.json"), filepath.Join(dir, "wiremock", "stubs.json")},
		{"scenario", cfg.ScenarioPath("happy-path.yaml"), filepath.Join(dir, "scenarios", "happy-path.yaml")},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s path = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestConfig_AbsoluteContentDirectory_IsUsedAsWritten keeps an absolute paths
// entry from being re-rooted under the config directory.
func TestConfig_AbsoluteContentDirectory_IsUsedAsWritten(t *testing.T) {
	dir := t.TempDir()
	shared := t.TempDir()
	path := filepath.Join(dir, "citadel.yaml")
	body := minimalConfig + "paths:\n  payloads: " + shared + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	want := filepath.Join(shared, "payload.json")
	if got := cfg.PayloadPath("payload.json"); got != want {
		t.Errorf("payload path = %q, want %q", got, want)
	}
}

// TestConfig_AbsoluteFile_IsUsedAsWritten keeps a scenario able to name a file
// outside the content tree.
func TestConfig_AbsoluteFile_IsUsedAsWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "citadel.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	absolute := filepath.Join(t.TempDir(), "payload.json")
	if got := cfg.PayloadPath(absolute); got != absolute {
		t.Errorf("payload path = %q, want %q", got, absolute)
	}
}

// TestConfig_WireMockDir_ResolvesAgainstTheConfigDirectory covers the directory
// the stub loader reads. The loader walks a directory rather than resolving one
// file, and reading paths.wiremock directly made it resolve against the working
// directory, so a run from the repository root found no pack and loaded nothing
// without reporting anything.
func TestConfig_WireMockDir_ResolvesAgainstTheConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "citadel.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got, want := cfg.WireMockDir(), filepath.Join(dir, "wiremock"); got != want {
		t.Errorf("WireMockDir() = %q, want %q", got, want)
	}
}

// TestConfig_WireMockDir_AbsoluteValue_IsUsedAsWritten keeps a shared pack
// outside the config directory reachable.
func TestConfig_WireMockDir_AbsoluteValue_IsUsedAsWritten(t *testing.T) {
	dir := t.TempDir()
	shared := t.TempDir()
	body := minimalConfig + "paths:\n  wiremock: " + shared + "\n"
	path := filepath.Join(dir, "citadel.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.WireMockDir(); got != shared {
		t.Errorf("WireMockDir() = %q, want %q", got, shared)
	}
}
