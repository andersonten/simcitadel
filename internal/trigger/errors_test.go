package trigger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/trigger"
	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// This file locks down the publish semantics of SDD section 7.5. These tests
// began as a proof that a 422 said only "unroutable", which hides the exchange
// and the routing key that the failure is about, and that a 502 said only
// "broker did not confirm", which hides the window that expired.

// publishTo sends one publish request and returns the recorder.
func publishTo(t *testing.T, srv *trigger.Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(validEvent()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", "run-1")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// TestPublishHandler_UnroutableMessage_ReportsExchangeAndRoutingKey holds the
// mandatory delivery rule of SDD section 7.5, which names both values as part
// of the 422.
func TestPublishHandler_UnroutableMessage_ReportsExchangeAndRoutingKey(t *testing.T) {
	cases := []struct {
		name   string
		result *citadelamqp.PublishResult
	}{
		{
			name: "the broker reports the return",
			result: &citadelamqp.PublishResult{
				Confirmed: true,
				Returned:  true,
				Return: &amqpdriver.Return{
					Exchange:   "test-exchange",
					RoutingKey: "test-key",
				},
			},
		},
		{
			name:   "the return carries no detail",
			result: &citadelamqp.PublishResult{Confirmed: true, Returned: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := trigger.NewServer(&mockPublisher{result: tc.result}, trigger.NewBudget(100), func() bool { return true })

			rr := publishTo(t, srv)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnprocessableEntity)
			}

			var resp map[string]string
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp["error"] != "unroutable" {
				t.Errorf("error = %q, want %q", resp["error"], "unroutable")
			}
			if resp["exchange"] != "test-exchange" {
				t.Errorf("exchange = %q, want the exchange of the publish", resp["exchange"])
			}
			if resp["routingKey"] != "test-key" {
				t.Errorf("routingKey = %q, want the routing key of the publish", resp["routingKey"])
			}
		})
	}
}

// TestPublishHandler_ConfirmTimeout_ReportsTheWindow holds the wording of SDD
// section 7.5: a timeout is a 502 that names the window that expired.
func TestPublishHandler_ConfirmTimeout_ReportsTheWindow(t *testing.T) {
	pub := &mockPublisher{err: fmt.Errorf("await publish confirm: %w", context.DeadlineExceeded)}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	rr := publishTo(t, srv)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadGateway)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "broker did not confirm within 5s" {
		t.Errorf("error = %q, want the message of SDD section 7.5 with the window", resp["error"])
	}
}

// TestPublishHandler_ConfirmTimeout_ReportsThePublisherWindow keeps the window
// in the message the one the publisher enforced, and not the default of the
// handler, so the number a reader sees is the number that expired.
func TestPublishHandler_ConfirmTimeout_ReportsThePublisherWindow(t *testing.T) {
	pub := &mockPublisher{err: &citadelamqp.ConfirmTimeoutError{Timeout: 7 * time.Second}}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	rr := publishTo(t, srv)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadGateway)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "broker did not confirm within 7s" {
		t.Errorf("error = %q, want the window the publisher enforced", resp["error"])
	}
}

// blockingPublisher waits for the caller to give up and then fails with a
// message that says nothing about a timeout. It proves the handler classifies a
// timeout from the failure itself and not from the words in it.
type blockingPublisher struct{}

func (blockingPublisher) Publish(ctx context.Context, _, _ string, _ amqpdriver.Publishing) (*citadelamqp.PublishResult, error) {
	<-ctx.Done()
	return nil, errors.New("channel/connection is not open")
}

// TestPublishHandler_ConfirmTimeout_IsFoundWithoutReadingTheErrorText keeps the
// classification off the error wording, which changes without notice.
func TestPublishHandler_ConfirmTimeout_IsFoundWithoutReadingTheErrorText(t *testing.T) {
	srv := trigger.NewServer(blockingPublisher{}, trigger.NewBudget(100), func() bool { return true })
	srv.SetConfirmTimeout(20 * time.Millisecond)

	rr := publishTo(t, srv)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadGateway)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "broker did not confirm within 20ms" {
		t.Errorf("error = %q, want the message of SDD section 7.5 with the window", resp["error"])
	}
}

// TestPublishHandler_BrokerNack_KeepsItsOwnMessage keeps the timeout branch
// from swallowing every other publish failure.
func TestPublishHandler_BrokerNack_KeepsItsOwnMessage(t *testing.T) {
	pub := &mockPublisher{err: errors.New("broker nack: message not confirmed")}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	rr := publishTo(t, srv)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadGateway)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "broker nack: message not confirmed" {
		t.Errorf("error = %q, want the message of the failure itself", resp["error"])
	}
}
