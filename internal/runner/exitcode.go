package runner

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
)

// ExitSuccess indicates all steps and assertions passed.
const ExitSuccess = 0

// ExitFailure indicates a step or assertion failed.
const ExitFailure = 1

// ExitInvalidInput indicates bad configuration, a bad scenario, or a missing file.
const ExitInvalidInput = 2

// ExitEnvironment indicates a target is unreachable, unhealthy, or misconfigured.
const ExitEnvironment = 3

// ExitTimeout indicates the run timeout exceeded.
const ExitTimeout = 4

// ExitLockHeld indicates another run holds the environment lock.
const ExitLockHeld = 5

// ExitInternal indicates a bug in Citadel.
const ExitInternal = 6

// InvalidInputError represents a configuration or validation error.
type InvalidInputError struct {
	Msg string
}

func (e *InvalidInputError) Error() string { return e.Msg }

// EnvironmentError represents a target connectivity or health error.
type EnvironmentError struct {
	Msg string
}

func (e *EnvironmentError) Error() string { return e.Msg }

// TimeoutError represents a run timeout.
type TimeoutError struct {
	Msg string
}

func (e *TimeoutError) Error() string { return e.Msg }

// LockHeldError represents a lock acquisition failure.
type LockHeldError struct {
	Msg string
}

func (e *LockHeldError) Error() string { return e.Msg }

// InternalError represents a bug in Citadel.
type InternalError struct {
	Msg string
}

func (e *InternalError) Error() string { return e.Msg }

// StepFailureError represents a step or assertion failure.
type StepFailureError struct {
	Msg string
}

func (e *StepFailureError) Error() string { return e.Msg }

// mergeExitCode combines the code a run already holds with the code of one more
// failure.
//
// SDD section 4.4 gives exit 1 to a finding about the application and calls it
// the only code with that meaning, so a later environment problem must not
// replace it. Without this rule the code of a run with several failures is the
// code of the last failure, which makes the answer depend on the order of the
// assertions rather than on what the run found.
func mergeExitCode(current, next int) int {
	if next == ExitSuccess {
		return current
	}
	if current == ExitSuccess || next == ExitFailure {
		return next
	}
	return current
}

// ClassifyError returns the appropriate exit code for the given error.
func ClassifyError(err error) int {
	if err == nil {
		return ExitSuccess
	}

	var inv *InvalidInputError
	if errors.As(err, &inv) {
		return ExitInvalidInput
	}

	var env *EnvironmentError
	if errors.As(err, &env) {
		return ExitEnvironment
	}

	var tmo *TimeoutError
	if errors.As(err, &tmo) {
		return ExitTimeout
	}

	var lck *LockHeldError
	if errors.As(err, &lck) {
		return ExitLockHeld
	}

	var intErr *InternalError
	if errors.As(err, &intErr) {
		return ExitInternal
	}

	var sf *StepFailureError
	if errors.As(err, &sf) {
		return ExitFailure
	}

	// A step package returns a plain wrapped error. SDD section 14 requires that
	// a step which cannot reach its target exits 3, so the transport errors are
	// classified here rather than falling through to "a bug in Citadel".
	if isTimeout(err) {
		return ExitTimeout
	}
	if isUnreachable(err) {
		return ExitEnvironment
	}
	if errors.Is(err, os.ErrNotExist) {
		return ExitInvalidInput
	}

	return ExitInternal
}

// isTimeout reports whether the error is a deadline or a network timeout.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isUnreachable reports whether the error is a failure to reach a target, as
// opposed to a target that answered with something unwanted.
func isUnreachable(err error) bool {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
