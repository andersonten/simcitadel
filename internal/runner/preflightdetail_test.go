package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// The event trigger and the capture service answer 503 while degraded, so a
// container health check carries a signal. Both preflight readers returned on any
// status but 200 before they read the body, and the body holds the reason. The
// 503 therefore traded the message a developer can act on for a status code.
//
// SDD section 4.10 requires preflight to fail with "a message that names the
// unreachable target and the check that failed". A status code names neither the
// broker nor the exchange.

// TestCheckHealthBrokerConnected_Degraded_NamesTheBroker proves the specific
// reason survives the 503.
func TestCheckHealthBrokerConnected_Degraded_NamesTheBroker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded","broker":"disconnected"}`))
	}))
	defer srv.Close()

	err := checkHealthBrokerConnected(context.Background(), srv.Client(), srv.URL, "event-trigger")
	if err == nil {
		t.Fatal("a degraded trigger passed preflight")
	}
	if !strings.Contains(err.Error(), "disconnected") {
		t.Errorf("the message does not name the broker state, so a developer cannot act on it: %v", err)
	}
}

// TestCheckSpyQueuesBound_Degraded_NamesTheExchange proves the same for the
// capture service, where the body names which exchange is wrong.
func TestCheckSpyQueuesBound_Degraded_NamesTheExchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"degraded","exchanges":{"public.a.v1":"bound","public.b.v1":"failed"}}`))
	}))
	defer srv.Close()

	exchanges := []config.CaptureExchange{{Name: "public.a.v1"}, {Name: "public.b.v1"}}
	err := checkSpyQueuesBound(context.Background(), srv.Client(), srv.URL, exchanges)
	if err == nil {
		t.Fatal("an unbound exchange passed preflight")
	}
	if !strings.Contains(err.Error(), "public.b.v1") {
		t.Errorf("the message does not name the unbound exchange: %v", err)
	}
	if strings.Contains(err.Error(), "public.a.v1") {
		t.Errorf("the message names the exchange that is bound, which sends the reader to the wrong place: %v", err)
	}
}

// TestPreflightHealth_UnexpectedStatus_StillFails keeps the change above from
// accepting a status that carries no health verdict at all. A 500 from a proxy or
// a 404 from a wrong URL is not a degraded service reporting itself.
func TestPreflightHealth_UnexpectedStatus_StillFails(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))

		if err := checkHealthBrokerConnected(context.Background(), srv.Client(), srv.URL, "event-trigger"); err == nil {
			t.Errorf("status %d passed the trigger check", code)
		}
		if err := checkSpyQueuesBound(context.Background(), srv.Client(), srv.URL, nil); err == nil {
			t.Errorf("status %d passed the capture check", code)
		}
		srv.Close()
	}
}
