package runner

import (
	"fmt"
	"testing"
)

func TestExitCode_Constants_MatchSDD(t *testing.T) {
	tests := []struct {
		name     string
		constant int
		want     int
	}{
		{"ExitSuccess", ExitSuccess, 0},
		{"ExitFailure", ExitFailure, 1},
		{"ExitInvalidInput", ExitInvalidInput, 2},
		{"ExitEnvironment", ExitEnvironment, 3},
		{"ExitTimeout", ExitTimeout, 4},
		{"ExitLockHeld", ExitLockHeld, 5},
		{"ExitInternal", ExitInternal, 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.want {
				t.Fatalf("%s = %d, want %d", tt.name, tt.constant, tt.want)
			}
		})
	}
}

func TestClassifyError_NilError_ReturnsSuccess(t *testing.T) {
	if got := ClassifyError(nil); got != ExitSuccess {
		t.Fatalf("ClassifyError(nil) = %d, want %d", got, ExitSuccess)
	}
}

func TestClassifyError_TypedErrors_ReturnCorrectCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"InvalidInputError", &InvalidInputError{Msg: "bad"}, ExitInvalidInput},
		{"EnvironmentError", &EnvironmentError{Msg: "down"}, ExitEnvironment},
		{"TimeoutError", &TimeoutError{Msg: "slow"}, ExitTimeout},
		{"LockHeldError", &LockHeldError{Msg: "locked"}, ExitLockHeld},
		{"InternalError", &InternalError{Msg: "bug"}, ExitInternal},
		{"StepFailureError", &StepFailureError{Msg: "fail"}, ExitFailure},
		{"wrapped InvalidInputError", fmt.Errorf("wrap: %w", &InvalidInputError{Msg: "bad"}), ExitInvalidInput},
		{"unknown error", fmt.Errorf("unknown"), ExitInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyError(tt.err); got != tt.want {
				t.Fatalf("ClassifyError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
