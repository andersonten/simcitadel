//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/runner"
)

// publishDirect publishes a body straight to an exchange over AMQP.
//
// The event trigger is not the only route to a broker. In production Debezium
// publishes from an outbox row, so no CloudEvents validation stands between the
// application and the exchange. A test that goes through the trigger cannot
// reach this path, because the trigger rejects a bad envelope with 400 and the
// message never arrives.
func publishDirect(t *testing.T, ch *amqpdriver.Channel, exchange, routingKey, runID, contentType string, body []byte) {
	t.Helper()

	err := ch.PublishWithContext(context.Background(), exchange, routingKey, false, false,
		amqpdriver.Publishing{
			ContentType: contentType,
			Headers:     amqpdriver.Table{"citadelrun": runID},
			Body:        body,
		})
	if err != nil {
		t.Fatalf("publish to exchange %s: %v", exchange, err)
	}
}

// capturedEntries reads the capture query response as a list of generic
// objects. The end-of-run conformance check reads the response the same way, so
// a response that this helper cannot parse is a response that the check cannot
// act on.
func capturedEntries(t *testing.T, captureURL, query string) []map[string]any {
	t.Helper()

	url := captureURL + "/captured"
	if query != "" {
		url += "?" + query
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("query captured messages: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("captured query returned status %d, expect %d", resp.StatusCode, http.StatusOK)
	}

	var entries []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("the capture query response is not valid JSON: %v", err)
	}
	return entries
}

// capturedCount reads the count endpoint, which the query API of SDD section
// 8.2 defines.
func capturedCount(t *testing.T, captureURL, query string) int {
	t.Helper()

	url := captureURL + "/captured/count"
	if query != "" {
		url += "?" + query
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("query the captured count: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]int
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode the captured count: %v", err)
	}
	return result["count"]
}

// runConformanceCheck runs the real end-of-run check of SDD section 4.11
// against the capture service. It calls runner.CheckConformance rather than
// repeat the rule, so the test covers the wiring and not a copy of the rule.
func runConformanceCheck(t *testing.T, captureURL string) error {
	t.Helper()

	cfg := &config.CitadelConfig{}
	cfg.Targets.EventCapture = &config.EventCaptureTarget{URL: captureURL}
	return runner.CheckConformance(context.Background(), cfg, &http.Client{Timeout: 10 * time.Second})
}

// cloudEventsValid reports the verdict that the capture service stored with the
// entry, per SDD section 8.2.
func cloudEventsValid(t *testing.T, entry map[string]any) bool {
	t.Helper()

	verdict, ok := entry["cloudEvents"].(map[string]any)
	if !ok {
		t.Fatalf("the captured entry holds cloudEvents %v, want an object", entry["cloudEvents"])
	}
	valid, _ := verdict["valid"].(bool)
	return valid
}

// SDD 14.2: a malformed envelope fails the run even with no assertion on it.
//
// The test publishes over AMQP and not through the event trigger, because the
// trigger validates the envelope and answers 400. A message that the trigger
// rejects never reaches a broker, so it cannot prove anything about the
// end-of-run check. Debezium reaches the exchange with no such gate.
//
// No assertion in this test names any of the three messages. The verdict comes
// from the capture service and from the conformance rule alone.
func TestConformance_MalformedEnvelopeOnTheExchange_FailsTheRun(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.Conformance.v1"
		queue      = "test.conformance.consumer"
		routingKey = "article.submitted"
		runID      = "run-conformance-0001"
		notJSON    = "<xml>not an event</xml>"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	// A conforming envelope. It is the control, because a test in which every
	// message fails proves nothing about the rule.
	conforming, err := json.Marshal(validEvent("e-conformance-ok"))
	if err != nil {
		t.Fatalf("marshal the conforming envelope: %v", err)
	}

	// Valid JSON without the required version attribute.
	noVersion, err := json.Marshal(map[string]any{
		"specversion": "1.0",
		"id":          "e-conformance-noversion",
		"type":        "ArticleSubmitted",
		"source":      "SRV-TEST",
		"data":        map[string]any{"articleId": "515150"},
	})
	if err != nil {
		t.Fatalf("marshal the envelope without a version: %v", err)
	}

	publishDirect(t, ch, exchange, routingKey, runID, "application/json", conforming)
	publishDirect(t, ch, exchange, routingKey, runID, "application/json", noVersion)
	publishDirect(t, ch, exchange, routingKey, runID, "application/xml", []byte(notJSON))

	waitFor(t, "all three messages are in the capture buffer", func() bool {
		return len(capturedEntries(t, captureURL, "runId="+runID)) == 3
	})

	// The count endpoint must see the malformed messages too. The runner counts
	// the captured messages of the run through this endpoint.
	if got := capturedCount(t, captureURL, "runId="+runID); got != 3 {
		t.Errorf("the count endpoint reports %d messages, want 3", got)
	}

	entries := capturedEntries(t, captureURL, "runId="+runID)
	for i, entry := range entries {
		t.Logf("captured entry %d: bodyEncoding=%v cloudEvents=%v body=%v",
			i, entry["bodyEncoding"], entry["cloudEvents"], entry["body"])
	}

	control, missingVersion, notAnEvent := entries[0], entries[1], entries[2]

	if !cloudEventsValid(t, control) {
		t.Errorf("the conforming envelope reports cloudEvents.valid false: %v", control["cloudEvents"])
	}
	if cloudEventsValid(t, missingVersion) {
		t.Error("the envelope without a version attribute reports cloudEvents.valid true, want false")
	}
	if cloudEventsValid(t, notAnEvent) {
		t.Error("the body that is not JSON reports cloudEvents.valid true, want false")
	}
	if got, ok := notAnEvent["body"].(string); !ok || got != notJSON {
		t.Errorf("the entry of the body that is not JSON holds body %v, want the verbatim bytes %q",
			notAnEvent["body"], notJSON)
	}

	// The real check must fail, and it must name each message it rejects. This
	// is the assertion the section 14.2 row asks for, because no step in this
	// test named any of the three messages.
	err = runConformanceCheck(t, captureURL)
	if err == nil {
		t.Fatal("the end-of-run conformance check passed although two of the three captured messages are malformed")
	}
	t.Logf("conformance failure: %v", err)

	for _, want := range []string{"e-conformance-noversion", "version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure text does not name %q, so it cannot lead a developer to the message: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "e-conformance-ok") {
		t.Errorf("the failure text names the conforming envelope: %v", err)
	}
}

// TestConformance_EveryEnvelopeConforms_PassesTheCheck keeps the test above
// from passing for the wrong reason. A check that fails on any input proves
// nothing about a malformed envelope.
func TestConformance_EveryEnvelopeConforms_PassesTheCheck(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.ConformanceOK.v1"
		queue      = "test.conformance.ok.consumer"
		routingKey = "article.submitted"
		runID      = "run-conformance-0002"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	conforming, err := json.Marshal(validEvent("e-conformance-only-good"))
	if err != nil {
		t.Fatalf("marshal the conforming envelope: %v", err)
	}
	publishDirect(t, ch, exchange, routingKey, runID, "application/json", conforming)

	waitFor(t, "the message is in the capture buffer", func() bool {
		return len(capturedEntries(t, captureURL, "runId="+runID)) == 1
	})

	if err := runConformanceCheck(t, captureURL); err != nil {
		t.Errorf("the conformance check failed on a run whose every envelope conforms: %v", err)
	}
}
