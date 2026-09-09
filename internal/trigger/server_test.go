package trigger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/trigger"
	amqpdriver "github.com/rabbitmq/amqp091-go"
)

type mockPublisher struct {
	result *citadelamqp.PublishResult
	err    error
}

func (m *mockPublisher) Publish(_ context.Context, _, _ string, _ amqpdriver.Publishing) (*citadelamqp.PublishResult, error) {
	return m.result, m.err
}

func validEvent() string {
	return `{"exchange":"test-exchange","routingKey":"test-key","event":{"specversion":"1.0","id":"evt-001","type":"TestEvent","source":"test-source","version":"1.0.0"}}`
}

func TestPublishHandler_ValidRequest_Returns202(t *testing.T) {
	pub := &mockPublisher{
		result: &citadelamqp.PublishResult{Confirmed: true},
	}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(validEvent()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", "run-1")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status code: want %d, got %d", http.StatusAccepted, rr.Code)
	}

	var resp struct {
		Published bool   `json:"published"`
		Confirmed bool   `json:"confirmed"`
		Routed    bool   `json:"routed"`
		EventID   string `json:"eventId"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Published {
		t.Fatal("published: want true, got false")
	}
	if !resp.Confirmed {
		t.Fatal("confirmed: want true, got false")
	}
	if !resp.Routed {
		t.Fatal("routed: want true, got false")
	}
	if resp.EventID != "evt-001" {
		t.Fatalf("eventId: want %q, got %q", "evt-001", resp.EventID)
	}
}

func TestPublishHandler_InvalidJSON_Returns400(t *testing.T) {
	pub := &mockPublisher{
		result: &citadelamqp.PublishResult{Confirmed: true},
	}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader("{not-json"))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status code: want %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestPublishHandler_MissingExchange_Returns400(t *testing.T) {
	pub := &mockPublisher{
		result: &citadelamqp.PublishResult{Confirmed: true},
	}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	body := `{"routingKey":"test-key","event":{"specversion":"1.0","id":"evt-001","type":"TestEvent","source":"test-source","version":"1.0.0"}}`
	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status code: want %d, got %d", http.StatusBadRequest, rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "exchange is required" {
		t.Fatalf("error: want %q, got %q", "exchange is required", resp["error"])
	}
}

func TestPublishHandler_InvalidCloudEvents_Returns400(t *testing.T) {
	pub := &mockPublisher{
		result: &citadelamqp.PublishResult{Confirmed: true},
	}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	body := `{"exchange":"test-exchange","routingKey":"test-key","event":{"source":"test-source","specversion":"1.0","id":"evt-001","version":"1.0.0"}}`
	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status code: want %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestPublishHandler_BudgetExceeded_Returns429(t *testing.T) {
	pub := &mockPublisher{
		result: &citadelamqp.PublishResult{Confirmed: true},
	}
	srv := trigger.NewServer(pub, trigger.NewBudget(0), func() bool { return true })

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(validEvent()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", "run-1")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status code: want %d, got %d", http.StatusTooManyRequests, rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "publish budget exhausted" {
		t.Fatalf("error: want %q, got %q", "publish budget exhausted", resp["error"])
	}
}

func TestPublishHandler_UnroutableMessage_Returns422(t *testing.T) {
	pub := &mockPublisher{
		result: &citadelamqp.PublishResult{Confirmed: true, Returned: true},
	}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(validEvent()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", "run-1")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status code: want %d, got %d", http.StatusUnprocessableEntity, rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "unroutable" {
		t.Fatalf("error: want %q, got %q", "unroutable", resp["error"])
	}
}

func TestPublishHandler_BrokerTimeout_Returns502(t *testing.T) {
	// The confirm window arrives as context.DeadlineExceeded. The handler no
	// longer looks for the word "timeout" in the message, which SDD section 7.5
	// does not fix and which changes without notice.
	pub := &mockPublisher{
		err: fmt.Errorf("await publish confirm: %w", context.DeadlineExceeded),
	}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(validEvent()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", "run-1")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status code: want %d, got %d", http.StatusBadGateway, rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != "broker did not confirm within 5s" {
		t.Fatalf("error: want %q, got %q", "broker did not confirm within 5s", resp["error"])
	}
}

func TestHealthHandler_Connected_ReturnsOK(t *testing.T) {
	pub := &mockPublisher{}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status code: want %d, got %d", http.StatusOK, rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("status: want %q, got %q", "ok", resp["status"])
	}
	if resp["broker"] != "connected" {
		t.Fatalf("broker: want %q, got %q", "connected", resp["broker"])
	}
}

func TestHealthHandler_Disconnected_ReturnsDegraded(t *testing.T) {
	pub := &mockPublisher{}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return false })

	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	// A degraded trigger answers 503. internal/health.Probe and the compose
	// health check both accept a 200 alone, so the earlier 200 reported a trigger
	// that cannot publish as healthy. See healthstatus_test.go in this package.
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status code: want %d, got %d", http.StatusServiceUnavailable, rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "degraded" {
		t.Fatalf("status: want %q, got %q", "degraded", resp["status"])
	}
	if resp["broker"] != "disconnected" {
		t.Fatalf("broker: want %q, got %q", "disconnected", resp["broker"])
	}
}

func TestBudgetResetHandler_SpecificRun_Returns200(t *testing.T) {
	pub := &mockPublisher{}
	budget := trigger.NewBudget(1)
	srv := trigger.NewServer(pub, budget, func() bool { return true })

	budget.Acquire("run-1")
	if budget.Count("run-1") != 1 {
		t.Fatal("precondition: run-1 count must be 1")
	}

	body := `{"runId":"run-1"}`
	req := httptest.NewRequest(http.MethodPost, "/budget/reset", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status code: want %d, got %d", http.StatusOK, rr.Code)
	}
	if budget.Count("run-1") != 0 {
		t.Fatalf("run-1 count after reset: want 0, got %d", budget.Count("run-1"))
	}
}

func TestBudgetResetHandler_AllRuns_Returns200(t *testing.T) {
	pub := &mockPublisher{}
	budget := trigger.NewBudget(1)
	srv := trigger.NewServer(pub, budget, func() bool { return true })

	budget.Acquire("run-1")
	budget.Acquire("run-2")

	req := httptest.NewRequest(http.MethodPost, "/budget/reset", nil)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status code: want %d, got %d", http.StatusOK, rr.Code)
	}
	if budget.Count("run-1") != 0 {
		t.Fatalf("run-1 count after reset all: want 0, got %d", budget.Count("run-1"))
	}
	if budget.Count("run-2") != 0 {
		t.Fatalf("run-2 count after reset all: want 0, got %d", budget.Count("run-2"))
	}
}
