package scenario

import (
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// This file locks down the checks of SDD section 13 that a scenario cannot
// verify on its own. These tests began as a proof that a ref: to nothing, an
// unknown retry.until, and a probe pointed at WireMock all passed validation.

// hasErrorContaining reports whether any error mentions the substring.
func hasErrorContaining(errs []error, want string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), want) {
			return true
		}
	}
	return false
}

// TestValidate_RefWithoutAPriorAs_IsRejected holds the second half of check 5.
func TestValidate_RefWithoutAPriorAs_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "dangling-ref",
		Steps: []Step{
			{
				Name:       "republish something that was never bound",
				Type:       "event/republish",
				Ref:        "accepted",
				Exchange:   "public.a.v1",
				RoutingKey: "a",
			},
		},
	}

	errs := Validate(s, "")
	if !hasErrorContaining(errs, `ref "accepted" does not name a prior as: binding`) {
		t.Errorf("a ref to no binding was accepted; errors: %v", errs)
	}
}

// TestValidate_RefToALaterAs_IsRejected keeps the ordering meaningful. A
// binding made after the reference does not exist when the step runs.
func TestValidate_RefToALaterAs_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "forward-ref",
		Steps: []Step{
			{Name: "use it early", Type: "event/republish", Ref: "accepted", Exchange: "e", RoutingKey: "r"},
			{Name: "bind it late", Type: "event/publish", As: "accepted", Exchange: "e", RoutingKey: "r", Data: map[string]any{}},
		},
	}

	if errs := Validate(s, ""); !hasErrorContaining(errs, `ref "accepted"`) {
		t.Errorf("a forward ref was accepted; errors: %v", errs)
	}
}

// TestValidate_RefToAPriorAs_IsAccepted keeps the check above from rejecting
// the documented shape of SDD section 6.4.
func TestValidate_RefToAPriorAs_IsAccepted(t *testing.T) {
	s := &Scenario{
		Name: "good-ref",
		Steps: []Step{
			{Name: "bind it", Type: "event/publish", As: "accepted", Exchange: "e", RoutingKey: "r", Data: map[string]any{}},
			{Name: "use it", Type: "event/republish", Ref: "accepted", Exchange: "e", RoutingKey: "r"},
		},
	}

	if errs := Validate(s, ""); hasErrorContaining(errs, "ref") {
		t.Errorf("a valid ref was rejected; errors: %v", errs)
	}
}

// TestValidate_UnknownRetryUntil_IsRejected holds SDD section 6.3, which
// defines exactly one value for Milestone 1.
func TestValidate_UnknownRetryUntil_IsRejected(t *testing.T) {
	s := &Scenario{
		Name: "bad-until",
		Steps: []Step{
			{
				Name:   "await with an invented until",
				Type:   "await",
				HTTP:   &HTTPSource{Path: "/x"},
				Expect: &Expect{Status: 200},
				Retry:  &RetrySpec{Until: "settled"},
			},
		},
	}

	if errs := Validate(s, ""); !hasErrorContaining(errs, `retry.until must be "pass"`) {
		t.Errorf("an unknown retry.until was accepted; errors: %v", errs)
	}
}

// TestValidate_UnparseableDurations_AreRejected keeps a bad duration from
// reaching the runner, where it would surface as an internal error mid-run.
func TestValidate_UnparseableDurations_AreRejected(t *testing.T) {
	cases := []struct {
		name string
		step Step
		want string
	}{
		{
			name: "step timeout",
			step: Step{
				Name: "bad step timeout", Type: "app/http", Timeout: "not-a-duration",
				HTTP: &HTTPSource{Path: "/x"}, Expect: &Expect{Status: 200},
			},
			want: `timeout "not-a-duration" is not a duration`,
		},
		{
			name: "retry timeout",
			step: Step{
				Name: "bad retry timeout", Type: "await",
				HTTP: &HTTPSource{Path: "/x"}, Expect: &Expect{Status: 200},
				Retry: &RetrySpec{Until: "pass", Timeout: "soon"},
			},
			want: `retry.timeout "soon" is not a duration`,
		},
		{
			name: "retry interval",
			step: Step{
				Name: "bad retry interval", Type: "await",
				HTTP: &HTTPSource{Path: "/x"}, Expect: &Expect{Status: 200},
				Retry: &RetrySpec{Until: "pass", Interval: "often"},
			},
			want: `retry.interval "often" is not a duration`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := Validate(&Scenario{Name: "durations", Steps: []Step{tc.step}}, "")
			if !hasErrorContaining(errs, tc.want) {
				t.Errorf("the bad duration was accepted; errors: %v", errs)
			}
		})
	}
}

// TestValidate_ValidDurations_AreAccepted keeps the duration check from
// rejecting a well-formed step.
func TestValidate_ValidDurations_AreAccepted(t *testing.T) {
	s := &Scenario{
		Name: "good-durations",
		Steps: []Step{{
			Name: "await with real durations", Type: "await", Timeout: "20s",
			HTTP: &HTTPSource{Path: "/x"}, Expect: &Expect{Status: 200},
			Retry: &RetrySpec{Until: "pass", Timeout: "30s", Interval: "250ms"},
		}},
	}

	if errs := Validate(s, ""); len(errs) != 0 {
		t.Errorf("a well-formed step was rejected: %v", errs)
	}
}

// TestValidateWithOptions_AppResolvingToWireMock_IsRejected holds check 6 of
// SDD section 13. The check compares resolved addresses, so an alias
// that points the app at the mock cannot defeat it.
func TestValidateWithOptions_AppResolvingToWireMock_IsRejected(t *testing.T) {
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{
				URL:      "http://127.0.0.1:9090",
				AdminURL: "http://127.0.0.1:9090/__admin",
			},
			// The app is named separately but resolves to the same address.
			App: &config.AppTarget{URL: "http://127.0.0.1:9090", GraphQLPath: "/graphql"},
		},
	}

	s := &Scenario{
		Name: "probe-the-mock",
		Steps: []Step{{
			Name:    "assert against what is really WireMock",
			Type:    "app/graphql",
			GraphQL: &GraphQLSource{Query: "{ __typename }"},
			Expect:  &Expect{Status: 200},
		}},
	}

	errs := ValidateWithOptions(s, Options{Config: cfg})
	if !hasErrorContaining(errs, "would assert against the mock") {
		t.Errorf("a probe against the WireMock address was accepted; errors: %v", errs)
	}
}

// TestValidateWithOptions_AppOnItsOwnAddress_IsAccepted keeps check 6 from
// rejecting a correctly separated environment.
func TestValidateWithOptions_AppOnItsOwnAddress_IsAccepted(t *testing.T) {
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{
				URL:      "http://127.0.0.1:9090",
				AdminURL: "http://127.0.0.1:9090/__admin",
			},
			App: &config.AppTarget{URL: "http://127.0.0.1:5000", GraphQLPath: "/graphql"},
		},
	}

	s := &Scenario{
		Name: "probe-the-app",
		Steps: []Step{{
			Name:    "assert against the app",
			Type:    "app/graphql",
			GraphQL: &GraphQLSource{Query: "{ __typename }"},
			Expect:  &Expect{Status: 200},
		}},
	}

	if errs := ValidateWithOptions(s, Options{Config: cfg}); len(errs) != 0 {
		t.Errorf("a probe against the app was rejected: %v", errs)
	}
}
