package runner

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteJUnit_ValidReport_ProducesValidXML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.xml")

	report := &JUnitReport{
		Name:     "test-suite",
		Tests:    2,
		Failures: 0,
		Time:     1.5,
		TestCases: []JUnitTestCase{
			{Name: "step-1", ClassName: "scenario-a", Time: 0.5},
			{Name: "step-2", ClassName: "scenario-a", Time: 1.0},
		},
	}

	if err := WriteJUnit(path, report); err != nil {
		t.Fatalf("WriteJUnit returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read junit file: %v", err)
	}

	if !strings.HasPrefix(string(data), xml.Header) {
		t.Fatal("junit file does not start with XML declaration")
	}

	var suites xmlTestSuites
	if err := xml.Unmarshal(data, &suites); err != nil {
		t.Fatalf("unmarshal junit xml: %v", err)
	}

	if len(suites.Suites) != 1 {
		t.Fatalf("suite count = %d, want 1", len(suites.Suites))
	}

	suite := suites.Suites[0]
	if suite.Name != "test-suite" {
		t.Fatalf("suite name = %q, want %q", suite.Name, "test-suite")
	}
	if suite.Tests != 2 {
		t.Fatalf("suite tests = %d, want 2", suite.Tests)
	}
	if len(suite.Cases) != 2 {
		t.Fatalf("case count = %d, want 2", len(suite.Cases))
	}
}

func TestWriteJUnit_WithFailure_IncludesFailureElement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fail.xml")

	report := &JUnitReport{
		Name:     "fail-suite",
		Tests:    1,
		Failures: 1,
		Time:     0.5,
		TestCases: []JUnitTestCase{
			{
				Name:      "failing-step",
				ClassName: "scenario-b",
				Time:      0.5,
				Failure: &JUnitFailure{
					Message: "expected OPEN, got null",
					Type:    "AssertionError",
					Text:    "step failed: expected OPEN, got null",
				},
			},
		},
	}

	if err := WriteJUnit(path, report); err != nil {
		t.Fatalf("WriteJUnit returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read junit file: %v", err)
	}

	var suites xmlTestSuites
	if err := xml.Unmarshal(data, &suites); err != nil {
		t.Fatalf("unmarshal junit xml: %v", err)
	}

	tc := suites.Suites[0].Cases[0]
	if tc.Failure == nil {
		t.Fatal("test case has no failure element")
	}
	if tc.Failure.Message != "expected OPEN, got null" {
		t.Fatalf("failure message = %q, want %q", tc.Failure.Message, "expected OPEN, got null")
	}
	if tc.Failure.Text != "step failed: expected OPEN, got null" {
		t.Fatalf("failure text = %q, want %q", tc.Failure.Text, "step failed: expected OPEN, got null")
	}
}
