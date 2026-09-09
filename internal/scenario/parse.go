package scenario

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Parse reads a scenario YAML file and returns the parsed Scenario.
// Parse rejects unknown keys at any level.
func Parse(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario file %s: %w", path, err)
	}
	s, err := ParseBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse scenario file %s: %w", path, err)
	}
	return s, nil
}

// ParseBytes parses scenario YAML from raw bytes.
// ParseBytes rejects unknown keys.
func ParseBytes(data []byte) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode scenario: %w", err)
	}

	if s.Name == "" {
		return nil, fmt.Errorf("scenario name is required")
	}
	if len(s.Steps) == 0 {
		return nil, fmt.Errorf("scenario %q has no steps", s.Name)
	}

	return &s, nil
}
