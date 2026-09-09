package config

import (
	"testing"
	"time"
)

// stubExit replaces the process exit function for one test and reports whether
// it was called. A test must never terminate the test binary.
func stubExit(t *testing.T) func() bool {
	t.Helper()
	original := exitFunc
	exited := false
	exitFunc = func(int) { exited = true }
	t.Cleanup(func() { exitFunc = original })
	return func() bool { return exited }
}

// TestOptInt_AbsentOrEmpty_ReturnsTheDefault holds the budget of SDD section 7.6
// to its documented default. The event trigger reads its budget through OptInt,
// so a deployment that sets no variable must get the default rather than zero,
// which would reject the first publish.
func TestOptInt_AbsentOrEmpty_ReturnsTheDefault(t *testing.T) {
	cases := map[string]struct {
		set   bool
		value string
	}{
		"absent": {set: false},
		"empty":  {set: true, value: ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			exited := stubExit(t)
			if tc.set {
				t.Setenv("CITADEL_TEST_BUDGET", tc.value)
			}

			if got := OptInt("CITADEL_TEST_BUDGET", 100); got != 100 {
				t.Errorf("OptInt = %d, want the default 100", got)
			}
			if exited() {
				t.Error("an unset variable must not end the process")
			}
		})
	}
}

// TestOptInt_ValidValue_OverridesTheDefault proves a deployment can raise the
// budget, which is what SDD section 14.5 row 14 needs when the same scenario
// runs under Helm with only a config change.
func TestOptInt_ValidValue_OverridesTheDefault(t *testing.T) {
	exited := stubExit(t)
	t.Setenv("CITADEL_TEST_BUDGET", "250")

	if got := OptInt("CITADEL_TEST_BUDGET", 100); got != 250 {
		t.Errorf("OptInt = %d, want 250", got)
	}
	if exited() {
		t.Error("a valid value must not end the process")
	}
}

// TestOptInt_UnusableValue_EndsTheProcess proves the failure is loud. A budget
// of zero or a typo must stop the service at startup, because a budget that
// silently reads as zero rejects every publish with 429 and looks like a broker
// fault.
func TestOptInt_UnusableValue_EndsTheProcess(t *testing.T) {
	for _, value := range []string{"not-a-number", "0", "-5", "12.5", " 7 "} {
		t.Run(value, func(t *testing.T) {
			exited := stubExit(t)
			t.Setenv("CITADEL_TEST_BUDGET", value)

			OptInt("CITADEL_TEST_BUDGET", 100)
			if !exited() {
				t.Errorf("the value %q is not a positive integer, so the process must end", value)
			}
		})
	}
}

// TestOptDuration_AbsentOrValid_BehavesAsDocumented covers the confirm timeout
// of SDD section 7.5, which reaches the event trigger the same way.
func TestOptDuration_AbsentOrValid_BehavesAsDocumented(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		exited := stubExit(t)
		if got := OptDuration("CITADEL_TEST_TIMEOUT", 5*time.Second); got != 5*time.Second {
			t.Errorf("OptDuration = %s, want the default 5s", got)
		}
		if exited() {
			t.Error("an unset variable must not end the process")
		}
	})

	t.Run("valid", func(t *testing.T) {
		exited := stubExit(t)
		t.Setenv("CITADEL_TEST_TIMEOUT", "7s")
		if got := OptDuration("CITADEL_TEST_TIMEOUT", 5*time.Second); got != 7*time.Second {
			t.Errorf("OptDuration = %s, want 7s", got)
		}
		if exited() {
			t.Error("a valid duration must not end the process")
		}
	})

	t.Run("unusable", func(t *testing.T) {
		exited := stubExit(t)
		t.Setenv("CITADEL_TEST_TIMEOUT", "soon")
		OptDuration("CITADEL_TEST_TIMEOUT", 5*time.Second)
		if !exited() {
			t.Error("a value that is not a duration must end the process")
		}
	})
}
