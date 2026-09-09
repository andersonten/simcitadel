package trigger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SDD section 11.1 asks for a healthcheck on every service and for
// service_healthy on every dependency, with no sleeps as a synchronisation
// mechanism. internal/health.Probe accepts a 200 and nothing else, so a
// degraded service that answers 200 reports healthy to Docker. A developer then
// reads "healthy" for a trigger that cannot publish, and the first step of the
// run fails instead.
//
// The status code carries the verdict. The body keeps the detail, because a
// developer needs to know which half is wrong.

func healthResponse(t *testing.T, s *Server) (int, map[string]string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode the health response: %v", err)
	}
	return rec.Code, body
}

// TestHealth_BrokerDisconnected_AnswersServiceUnavailable proves a trigger that
// cannot publish reports itself unhealthy.
func TestHealth_BrokerDisconnected_AnswersServiceUnavailable(t *testing.T) {
	s := &Server{connected: func() bool { return false }}

	code, body := healthResponse(t, s)
	if code != http.StatusServiceUnavailable {
		t.Errorf("a disconnected broker answers status %d, want %d", code, http.StatusServiceUnavailable)
	}
	if body["status"] != "degraded" {
		t.Errorf("the body reports status %q, want \"degraded\"", body["status"])
	}
	if body["broker"] != "disconnected" {
		t.Errorf("the body reports broker %q, want \"disconnected\"", body["broker"])
	}
}

// TestHealth_BrokerConnected_AnswersOK keeps the test above from passing for the
// wrong reason.
func TestHealth_BrokerConnected_AnswersOK(t *testing.T) {
	s := &Server{connected: func() bool { return true }}

	code, body := healthResponse(t, s)
	if code != http.StatusOK {
		t.Errorf("a connected broker answers status %d, want %d", code, http.StatusOK)
	}
	if body["status"] != "ok" {
		t.Errorf("the body reports status %q, want \"ok\"", body["status"])
	}
}
