//go:build integration

package integration

import (
	"net/http"
	"testing"
)

// SDD 14.2: Publish to an exchange with no binding -> 422 with exchange and
// routing key.
func TestPublish_ExchangeWithNoBinding_Returns422(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const exchange = "public.test.Unbound.v1"
	declareTopology(t, ch, exchange, "", "")

	triggerURL := startTriggerServer(t, amqpURL, 100).URL

	status, body := postPublish(t, triggerURL, "run-unbound", publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: "article.submitted",
		Event:      validEvent("e-unbound"),
	})

	if status != http.StatusUnprocessableEntity {
		t.Fatalf("publish to an unbound exchange returned status %d, expect %d; body=%v",
			status, http.StatusUnprocessableEntity, body)
	}
	if body["error"] == nil {
		t.Errorf("response has no error field; body=%v", body)
	}

	// SDD 7.5 and 14.2 both make the exchange and the routing key part of the
	// 422. "unroutable" alone does not say which binding is absent.
	if got, _ := body["exchange"].(string); got != exchange {
		t.Errorf("response reports exchange %q, want %q; body=%v", got, exchange, body)
	}
	if got, _ := body["routingKey"].(string); got != "article.submitted" {
		t.Errorf("response reports routingKey %q, want %q; body=%v", got, "article.submitted", body)
	}
}

// SDD 14.2: Publish to a bound exchange -> confirmed, captured, run passes.
func TestPublish_BoundExchange_IsConfirmedAndCaptured(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.Article.v1"
		queue      = "test.article.consumer"
		routingKey = "article.submitted"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL
	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	const runID = "run-bound-0001"
	status, body := postPublish(t, triggerURL, runID, publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: routingKey,
		Event:      validEvent("e-bound"),
	})

	if status != http.StatusAccepted {
		t.Fatalf("publish to a bound exchange returned status %d, expect %d; body=%v",
			status, http.StatusAccepted, body)
	}
	if confirmed, _ := body["confirmed"].(bool); !confirmed {
		t.Errorf("response reports confirmed=%v, expect true; body=%v", body["confirmed"], body)
	}
	if routed, _ := body["routed"].(bool); !routed {
		t.Errorf("response reports routed=%v, expect true; body=%v", body["routed"], body)
	}

	waitFor(t, "the published message is captured", func() bool {
		return len(capturedMessages(t, captureURL, "exchange="+exchange)) == 1
	})

	messages := capturedMessages(t, captureURL, "exchange="+exchange)
	if got := messages[0].RoutingKey; got != routingKey {
		t.Errorf("captured routing key = %q, want %q", got, routingKey)
	}
}

// TestPublish_RunIDFilter guards the defect where the capture service read the
// run ID from a header named "runId" while the trigger stamps "citadelrun".
// The filter therefore matched nothing, and every runId-scoped assertion
// silently saw an empty result.
func TestPublish_CapturedMessage_CarriesRunIDAndMatchesFilter(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.RunScope.v1"
		queue      = "test.runscope.consumer"
		routingKey = "article.submitted"
		runID      = "0191c3aa-0000-7000-8000-00000000abcd"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL
	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	status, body := postPublish(t, triggerURL, runID, publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: routingKey,
		Event:      validEvent("e-runscope"),
	})
	if status != http.StatusAccepted {
		t.Fatalf("publish returned status %d, expect %d; body=%v", status, http.StatusAccepted, body)
	}

	waitFor(t, "the published message is captured", func() bool {
		return len(capturedMessages(t, captureURL, "exchange="+exchange)) == 1
	})

	captured := capturedMessages(t, captureURL, "exchange="+exchange)[0]
	if captured.RunID != runID {
		t.Errorf("captured runId = %q, want %q; headers=%v", captured.RunID, runID, captured.Headers)
	}

	matching := capturedMessages(t, captureURL, "runId="+runID)
	if len(matching) != 1 {
		t.Errorf("runId filter returned %d messages, want 1", len(matching))
	}

	other := capturedMessages(t, captureURL, "runId=run-that-does-not-exist")
	if len(other) != 0 {
		t.Errorf("runId filter for an unknown run returned %d messages, want 0", len(other))
	}
}

// TestPublish_BudgetExhausted guards the closed-loop budget of SDD 7.5.
func TestPublish_BudgetExhausted_Returns429(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.Budget.v1"
		queue      = "test.budget.consumer"
		routingKey = "article.submitted"
		runID      = "run-budget-0001"
		budget     = 3
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	triggerURL := startTriggerServer(t, amqpURL, budget).URL

	for i := range budget {
		status, body := postPublish(t, triggerURL, runID, publishRequest{
			Delivery:   "amqp",
			Exchange:   exchange,
			RoutingKey: routingKey,
			Event:      validEvent("e-budget"),
		})
		if status != http.StatusAccepted {
			t.Fatalf("publish %d of %d returned status %d, expect %d; body=%v",
				i+1, budget, status, http.StatusAccepted, body)
		}
	}

	status, body := postPublish(t, triggerURL, runID, publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: routingKey,
		Event:      validEvent("e-budget-over"),
	})
	if status != http.StatusTooManyRequests {
		t.Fatalf("publish past the budget returned status %d, expect %d; body=%v",
			status, http.StatusTooManyRequests, body)
	}
}

// SDD 14.2: A malformed envelope fails the run even with no assertion on it.
func TestPublish_MalformedEnvelope_IsRejected(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.Malformed.v1"
		queue      = "test.malformed.consumer"
		routingKey = "article.submitted"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL

	cases := []struct {
		name  string
		event map[string]any
	}{
		{"no specversion", map[string]any{
			"id": "e1", "type": "ArticleSubmitted", "source": "SRV-TEST", "version": "v1"}},
		{"wrong specversion", map[string]any{
			"specversion": "0.3", "id": "e1", "type": "ArticleSubmitted", "source": "SRV-TEST", "version": "v1"}},
		{"no id", map[string]any{
			"specversion": "1.0", "type": "ArticleSubmitted", "source": "SRV-TEST", "version": "v1"}},
		{"no type", map[string]any{
			"specversion": "1.0", "id": "e1", "source": "SRV-TEST", "version": "v1"}},
		{"no source", map[string]any{
			"specversion": "1.0", "id": "e1", "type": "ArticleSubmitted", "version": "v1"}},
		{"id is not a string", map[string]any{
			"specversion": "1.0", "id": 42, "type": "ArticleSubmitted", "source": "SRV-TEST", "version": "v1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postPublish(t, triggerURL, "run-malformed", publishRequest{
				Delivery:   "amqp",
				Exchange:   exchange,
				RoutingKey: routingKey,
				Event:      tc.event,
			})
			if status != http.StatusBadRequest {
				t.Errorf("publish of a malformed envelope returned status %d, expect %d; body=%v",
					status, http.StatusBadRequest, body)
			}
		})
	}
}
