package steps

import (
	"sort"
	"testing"
)

func TestStepTypes_MatchesSDD_ClosedSet(t *testing.T) {
	expected := []string{
		"app/graphql",
		"app/http",
		"await",
		"event/captured",
		"event/not-captured",
		"event/publish",
		"event/republish",
		"inbox/assert",
		"probe",
		"sql/exec",
		"sql/query",
		"wait",
		"wiremock/set-state",
	}

	got := StepTypes()
	sort.Strings(got)
	sort.Strings(expected)

	if len(got) != len(expected) {
		t.Fatalf("expected %d types, got %d: %v", len(expected), len(got), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Errorf("index %d: expected %q, got %q", i, expected[i], got[i])
		}
	}
}

func TestIsValidType_ValidType_ReturnsTrue(t *testing.T) {
	cases := []string{"event/publish", "probe", "await", "wait", "sql/query"}
	for _, c := range cases {
		if !IsValidType(c) {
			t.Errorf("expected %q to be valid", c)
		}
	}
}

func TestIsValidType_InvalidType_ReturnsFalse(t *testing.T) {
	cases := []string{"", "unknown", "http/get", "sql", "event"}
	for _, c := range cases {
		if IsValidType(c) {
			t.Errorf("expected %q to be invalid", c)
		}
	}
}
