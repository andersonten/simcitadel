package capture

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func testServer(buf *Buffer, statuses map[string]string) *Server {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	statusFn := func() map[string]string {
		return statuses
	}
	return NewServer(buf, statusFn, logger)
}

func TestCapturedHandler_NoFilter_ReturnsAll(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", ReceivedAt: time.Now(), Body: json.RawMessage(`{"id":"1"}`)})
	buf.Add(Message{Exchange: "b", ReceivedAt: time.Now(), Body: json.RawMessage(`{"id":"2"}`)})

	srv := testServer(buf, map[string]string{})
	req := httptest.NewRequest(http.MethodGet, "/captured", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var messages []Message
	if err := json.NewDecoder(w.Body).Decode(&messages); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
}

func TestCapturedHandler_WithExchangeFilter_FiltersResults(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "alpha", ReceivedAt: time.Now(), Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "beta", ReceivedAt: time.Now(), Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "alpha", ReceivedAt: time.Now(), Body: json.RawMessage(`{}`)})

	srv := testServer(buf, map[string]string{})
	req := httptest.NewRequest(http.MethodGet, "/captured?exchange=alpha", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var messages []Message
	if err := json.NewDecoder(w.Body).Decode(&messages); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
	for _, msg := range messages {
		if msg.Exchange != "alpha" {
			t.Errorf("expected exchange %q, got %q", "alpha", msg.Exchange)
		}
	}
}

func TestCapturedCountHandler_ReturnsCount(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "b", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(`{}`)})

	srv := testServer(buf, map[string]string{})
	req := httptest.NewRequest(http.MethodGet, "/captured/count?exchange=a", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result map[string]int
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["count"] != 2 {
		t.Errorf("expected count 2, got %d", result["count"])
	}
}

func TestDeleteCapturedHandler_ClearsBuffer(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "b", Body: json.RawMessage(`{}`)})

	srv := testServer(buf, map[string]string{})
	req := httptest.NewRequest(http.MethodDelete, "/captured", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d", w.Code)
	}

	remaining := buf.Query(Filter{})
	if len(remaining) != 0 {
		t.Errorf("expected 0 messages after delete, got %d", len(remaining))
	}
}

func TestHealthHandler_AllBound_ReturnsOK(t *testing.T) {
	buf := NewBuffer(10)
	statuses := map[string]string{
		"exchange.a": "bound",
		"exchange.b": "bound",
	}

	srv := testServer(buf, statuses)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result map[string]json.RawMessage
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var status string
	if err := json.Unmarshal(result["status"], &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status != "ok" {
		t.Errorf("expected status %q, got %q", "ok", status)
	}

	var exchanges map[string]string
	if err := json.Unmarshal(result["exchanges"], &exchanges); err != nil {
		t.Fatalf("decode exchanges: %v", err)
	}
	if exchanges["exchange.a"] != "bound" {
		t.Errorf("expected exchange.a status %q, got %q", "bound", exchanges["exchange.a"])
	}
	if exchanges["exchange.b"] != "bound" {
		t.Errorf("expected exchange.b status %q, got %q", "bound", exchanges["exchange.b"])
	}
}
