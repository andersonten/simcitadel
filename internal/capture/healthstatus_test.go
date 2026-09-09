package capture

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A capture service that has not bound every exchange captures nothing on the
// exchange it missed, and preflight of SDD section 4.10 refuses to run against
// it. The health endpoint answered 200 in that state, so Docker reported the
// container healthy and the stack looked ready.
//
// SDD section 11.1 builds the start-up order out of service_healthy conditions.
// A health endpoint that always answers 200 makes every such condition pass the
// moment the listener opens, which removes the signal the condition exists for.

func healthResponse(t *testing.T, status map[string]string) (int, map[string]any) {
	t.Helper()

	s := &Server{
		buf:    NewBuffer(10),
		logger: slog.Default(),
		status: func() map[string]string { return status },
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode the health response: %v", err)
	}
	return rec.Code, body
}

// TestHealth_EveryExchangeBound_AnswersOK is the control.
func TestHealth_EveryExchangeBound_AnswersOK(t *testing.T) {
	code, body := healthResponse(t, map[string]string{
		"public.orchestration-engine.Manuscript.v1": "bound",
		"public.orchestration-engine.Task.v1":       "bound",
	})

	if code != http.StatusOK {
		t.Errorf("a fully bound service answers status %d, want %d", code, http.StatusOK)
	}
	if body["status"] != "ok" {
		t.Errorf("the body reports status %v, want \"ok\"", body["status"])
	}
}

// TestHealth_ExchangeNotBound_AnswersServiceUnavailable covers the states a
// developer must not read as ready.
func TestHealth_ExchangeNotBound_AnswersServiceUnavailable(t *testing.T) {
	cases := map[string]map[string]string{
		"one exchange is pending": {
			"public.orchestration-engine.Manuscript.v1": "bound",
			"public.orchestration-engine.Task.v1":       "pending",
		},
		"one exchange failed": {
			"public.orchestration-engine.Manuscript.v1": "failed",
		},
	}

	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			code, body := healthResponse(t, status)
			if code != http.StatusServiceUnavailable {
				t.Errorf("status %d, want %d", code, http.StatusServiceUnavailable)
			}
			if body["status"] != "degraded" {
				t.Errorf("the body reports status %v, want \"degraded\"", body["status"])
			}
			if _, ok := body["exchanges"]; !ok {
				t.Error("the body carries no exchanges map, so a developer cannot tell which one is wrong")
			}
		})
	}
}

// TestHealth_ConfiguredWithNoExchange_AnswersOK draws the line of this check.
// The service knows the list it was given and nothing else, so it cannot tell a
// deliberate empty list from a list that should hold two names. Only citadel
// doctor and preflight read capture.exchanges of the config and make that
// comparison, per SDD sections 12.3 and 4.10. A 503 here would make a legitimate
// empty configuration permanently unhealthy and would put the check in the wrong
// process.
func TestHealth_ConfiguredWithNoExchange_AnswersOK(t *testing.T) {
	code, body := healthResponse(t, map[string]string{})
	if code != http.StatusOK {
		t.Errorf("a service configured with no exchange answers status %d, want %d", code, http.StatusOK)
	}
	if body["status"] != "ok" {
		t.Errorf("the body reports status %v, want \"ok\"", body["status"])
	}
}
