package capture

import "testing"

// SDD section 4.3 stamps the run ID into every message the event trigger
// publishes, and it filters captured messages on that attribute by default. The
// reasoning is sound for an inbound event, and it excluded the outbound half of
// the platform entirely.
//
// An outbound event is produced by the application under test, through its outbox
// and Debezium. The application knows nothing about Citadel, which is the premise
// of the whole platform, so it stamps nothing. The filter therefore rejected every
// event the application produced, and the reference scenario failed with
// "countAtLeast: expected at least 1, got 0" while the event sat in the buffer.
//
// A message with no run ID matches a run-scoped query. That is safe rather than
// loose: section 4.8 holds an exclusive lock so one run uses an environment at a
// time, and section 4.9 purges every queue and clears the capture buffer before
// every scenario. An unstamped message in the buffer therefore arrived during this
// run. The alternative, which is to require the application to propagate a Citadel
// attribute, would mean the platform only works against an application modified
// for it.

// TestFilterMatch_MessageWithNoRunID_MatchesARunScopedQuery covers the event the
// application under test produces.
func TestFilterMatch_MessageWithNoRunID_MatchesARunScopedQuery(t *testing.T) {
	msg := Message{
		Exchange:   "public.orchestration-engine.Manuscript.v1",
		RoutingKey: "public.orchestration-engine.Manuscript.v1",
		Headers:    map[string]string{"eventType": "Manuscript.Created"},
		RunID:      "",
	}

	filter := Filter{
		Exchange:  "public.orchestration-engine.Manuscript.v1",
		EventType: "Manuscript.Created",
		RunID:     "01a01f2d-bd9c-77cc-9f77-90fabee1c2c5",
	}

	if !filter.Match(msg) {
		t.Error("a run-scoped query rejected an event that the application produced through its outbox")
	}
}

// TestFilterMatch_MessageFromAnotherRun_IsStillRejected keeps the exclusion that
// section 4.3 asks for. A stamped message names its run, so a mismatch is real
// evidence that the message belongs to a different run.
func TestFilterMatch_MessageFromAnotherRun_IsStillRejected(t *testing.T) {
	msg := Message{
		Exchange: "public.orchestration-engine.Manuscript.v1",
		RunID:    "01a01f00-0000-0000-0000-000000000000",
	}

	filter := Filter{
		Exchange: "public.orchestration-engine.Manuscript.v1",
		RunID:    "01a01f2d-bd9c-77cc-9f77-90fabee1c2c5",
	}

	if filter.Match(msg) {
		t.Error("a run-scoped query accepted a message stamped with a different run")
	}
}

// TestFilterMatch_UnscopedQuery_MatchesEveryRun keeps the debugging case working.
func TestFilterMatch_UnscopedQuery_MatchesEveryRun(t *testing.T) {
	stamped := Message{Exchange: "x", RunID: "01a01f00-0000-0000-0000-000000000000"}
	unstamped := Message{Exchange: "x"}

	filter := Filter{Exchange: "x"}

	if !filter.Match(stamped) || !filter.Match(unstamped) {
		t.Error("a query with no run ID must match a message whether or not it carries one")
	}
}
