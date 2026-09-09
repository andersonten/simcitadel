//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
	"github.com/andersonten/simcitadel/internal/steps"
)

// notCapturedContext builds a RunContext pointed at a live capture service.
func notCapturedContext(t *testing.T, captureURL, runID string, runStart time.Time) *steps.RunContext {
	t.Helper()

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "integration"},
		Targets: config.TargetsConfig{
			EventCapture: &config.EventCaptureTarget{URL: captureURL},
		},
	}

	rc := steps.NewRunContext(cfg, runID)
	rc.RunStart = runStart
	return rc
}

// SDD 14.2: event/not-captured with a message present before the assertion
// -> fails.
//
// This case only has meaning once the captured run ID matches the run ID that
// the step filters on. While the capture service read the wrong header the
// filter matched nothing and this assertion passed no matter what arrived.
func TestNotCaptured_MessagePresentBeforeAssertion_Fails(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.NotCapturedPre.v1"
		queue      = "test.notcaptured.pre.consumer"
		routingKey = "article.submitted"
		runID      = "run-notcaptured-pre"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	runStart := time.Now().Add(-time.Second)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL
	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	status, body := postPublish(t, triggerURL, runID, publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: routingKey,
		Event:      validEvent("e-notcaptured-pre"),
	})
	if status != http.StatusAccepted {
		t.Fatalf("publish returned status %d, expect %d; body=%v", status, http.StatusAccepted, body)
	}

	waitFor(t, "the message is in the capture buffer", func() bool {
		return len(capturedMessages(t, captureURL, "runId="+runID)) == 1
	})

	step := &scenario.Step{
		Name:        "no article submitted event",
		Type:        "event/not-captured",
		Exchange:    exchange,
		QuietPeriod: "500ms",
	}

	rc := notCapturedContext(t, captureURL, runID, runStart)
	result, err := steps.Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute returned an error, want a failed result: %v", err)
	}
	if result.Pass {
		t.Fatal("event/not-captured passed while a matching message was already present, want a failure")
	}
}

// SDD 14.2: event/not-captured with a matching message arriving during the
// quiet period -> fails.
func TestNotCaptured_MessageArrivesDuringQuietPeriod_Fails(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.NotCapturedMid.v1"
		queue      = "test.notcaptured.mid.consumer"
		routingKey = "article.submitted"
		runID      = "run-notcaptured-mid"
		quiet      = 4 * time.Second
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	runStart := time.Now().Add(-time.Second)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL
	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	// The buffer must be empty when the step makes its first query, so the
	// publish happens after the step starts.
	published := make(chan struct{})
	go func() {
		defer close(published)
		time.Sleep(quiet / 4)
		postPublish(t, triggerURL, runID, publishRequest{
			Delivery:   "amqp",
			Exchange:   exchange,
			RoutingKey: routingKey,
			Event:      validEvent("e-notcaptured-mid"),
		})
	}()
	t.Cleanup(func() { <-published })

	step := &scenario.Step{
		Name:        "no article submitted event",
		Type:        "event/not-captured",
		Exchange:    exchange,
		QuietPeriod: quiet.String(),
	}

	rc := notCapturedContext(t, captureURL, runID, runStart)
	result, err := steps.Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute returned an error, want a failed result: %v", err)
	}
	if result.Pass {
		t.Fatal("event/not-captured passed while a matching message arrived during the quiet period, want a failure")
	}
}

// TestNotCaptured_NoMatchingMessage_Passes is the control for the two failure
// cases. Without it a step that always fails would satisfy them both.
func TestNotCaptured_NoMatchingMessage_Passes(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.NotCapturedNone.v1"
		queue      = "test.notcaptured.none.consumer"
		routingKey = "article.submitted"
		runID      = "run-notcaptured-none"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	step := &scenario.Step{
		Name:        "no article submitted event",
		Type:        "event/not-captured",
		Exchange:    exchange,
		QuietPeriod: "500ms",
	}

	rc := notCapturedContext(t, captureURL, runID, time.Now().Add(-time.Second))
	result, err := steps.Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.Pass {
		t.Fatalf("event/not-captured failed with an empty buffer, want a pass: %v", result.Err)
	}
}
