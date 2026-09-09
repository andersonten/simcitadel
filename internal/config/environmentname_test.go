package config

import (
	"strings"
	"testing"
)

// TestLoad_ConfigWithoutEnvironmentName_IsRejected holds the identity that SDD
// section 4.8 puts in the lock-held message and that section 5.1 makes
// mandatory. Every other fixture supplies a name, so without this test the
// check can disappear and leave the suite green.
func TestLoad_ConfigWithoutEnvironmentName_IsRejected(t *testing.T) {
	cases := map[string]string{
		"the block is absent": "version: 1\n",
		"the name is empty":   "version: 1\nenvironment:\n  name: \"\"\n",
		"the block is empty":  "version: 1\nenvironment: {}\n",
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, content))
			if err == nil {
				t.Fatal("a config with no environment name must not load")
			}
			if !strings.Contains(err.Error(), "environment.name") {
				t.Errorf("the error must name the missing key, got %q", err.Error())
			}
		})
	}
}

// TestLoad_ConfigWithoutVersion_IsRejected covers the other mandatory key of
// section 5.1, for the same reason.
func TestLoad_ConfigWithoutVersion_IsRejected(t *testing.T) {
	_, err := Load(writeConfig(t, "environment:\n  name: local\n"))
	if err == nil {
		t.Fatal("a config with no version must not load")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("the error must name the missing key, got %q", err.Error())
	}
}

// TestLoad_ConfigWithVersionAndEnvironmentName_Loads is the positive control. It
// keeps the two checks above from passing for the wrong reason, such as a
// decoder that rejects every minimal file.
func TestLoad_ConfigWithVersionAndEnvironmentName_Loads(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version: 1\nenvironment:\n  name: local\n"))
	if err != nil {
		t.Fatalf("a minimal config must load: %v", err)
	}
	if cfg.Environment.Name != "local" {
		t.Errorf("environment.name = %q, want local", cfg.Environment.Name)
	}
}
