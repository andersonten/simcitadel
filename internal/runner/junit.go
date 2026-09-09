package runner

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

// JUnitReport holds a JUnit test suite.
type JUnitReport struct {
	Name      string
	Tests     int
	Failures  int
	Time      float64
	TestCases []JUnitTestCase
}

// JUnitTestCase represents one step or assertion result.
type JUnitTestCase struct {
	Name      string
	ClassName string
	Time      float64
	Failure   *JUnitFailure
}

// JUnitFailure holds failure details for a test case.
type JUnitFailure struct {
	Message string
	Type    string
	Text    string
}

type xmlTestSuites struct {
	XMLName xml.Name       `xml:"testsuites"`
	Suites  []xmlTestSuite `xml:"testsuite"`
}

type xmlTestSuite struct {
	Name     string        `xml:"name,attr"`
	Tests    int           `xml:"tests,attr"`
	Failures int           `xml:"failures,attr"`
	Time     float64       `xml:"time,attr"`
	Cases    []xmlTestCase `xml:"testcase"`
}

type xmlTestCase struct {
	Name      string      `xml:"name,attr"`
	ClassName string      `xml:"classname,attr"`
	Time      float64     `xml:"time,attr"`
	Failure   *xmlFailure `xml:"failure,omitempty"`
}

type xmlFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit writes a JUnit XML file that holds one test suite.
func WriteJUnit(path string, report *JUnitReport) error {
	return WriteJUnitSuites(path, []*JUnitReport{report})
}

// WriteJUnitSuites writes a JUnit XML file that holds one test suite per
// report. SDD section 4.6 renders a scenario as a test suite, so a run of
// several scenarios writes several suites into one file.
func WriteJUnitSuites(path string, reports []*JUnitReport) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create junit directory %s: %w", dir, err)
	}

	suites := xmlTestSuites{Suites: make([]xmlTestSuite, 0, len(reports))}
	for _, report := range reports {
		if report == nil {
			continue
		}
		suites.Suites = append(suites.Suites, xmlSuite(report))
	}

	data, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal junit xml: %w", err)
	}

	content := append([]byte(xml.Header), data...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write junit file %s: %w", path, err)
	}
	return nil
}

// xmlSuite renders one report as the test suite element of the JUnit format.
func xmlSuite(report *JUnitReport) xmlTestSuite {
	cases := make([]xmlTestCase, len(report.TestCases))
	for i, tc := range report.TestCases {
		c := xmlTestCase{
			Name:      tc.Name,
			ClassName: tc.ClassName,
			Time:      tc.Time,
		}
		if tc.Failure != nil {
			c.Failure = &xmlFailure{
				Message: tc.Failure.Message,
				Type:    tc.Failure.Type,
				Text:    tc.Failure.Text,
			}
		}
		cases[i] = c
	}

	return xmlTestSuite{
		Name:     report.Name,
		Tests:    report.Tests,
		Failures: report.Failures,
		Time:     report.Time,
		Cases:    cases,
	}
}
