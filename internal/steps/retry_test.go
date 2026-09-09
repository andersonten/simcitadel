package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

func TestExecuteWithRetry_PassesFirstTime_NoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "abc"}})
	}))
	defer srv.Close()

	step := &scenario.Step{
		Name: "test-probe",
		Type: "probe",
		HTTP: &scenario.HTTPSource{
			Method: "GET",
			Path:   "/test",
		},
		Expect: &scenario.Expect{
			Status: 200,
		},
	}

	rc := &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				App: &config.AppTarget{URL: srv.URL},
			},
		},
		HTTPClient: srv.Client(),
		Bindings:   make(map[string]any),
	}

	defaults := config.DefaultsConfig{}
	result, err := ExecuteWithRetry(context.Background(), step, rc, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Pass {
		t.Fatalf("expected pass, got: %s", result.Message)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", calls.Load())
	}
}

func TestExecuteWithRetry_FailsThenPasses_RetriesUntilPass(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]any{"error": "not ready"})
			return
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	step := &scenario.Step{
		Name: "test-retry",
		Type: "await",
		HTTP: &scenario.HTTPSource{
			Method: "GET",
			Path:   "/health",
		},
		Expect: &scenario.Expect{
			Status: 200,
		},
		Retry: &scenario.RetrySpec{
			Until:    "pass",
			Timeout:  "5s",
			Interval: "10ms",
		},
	}

	rc := &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				App: &config.AppTarget{URL: srv.URL},
			},
		},
		HTTPClient: srv.Client(),
		Bindings:   make(map[string]any),
	}

	defaults := config.DefaultsConfig{}
	result, err := ExecuteWithRetry(context.Background(), step, rc, defaults)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Pass {
		t.Fatalf("expected pass after retries, got: %s", result.Message)
	}
	if calls.Load() < 3 {
		t.Fatalf("expected at least 3 calls, got %d", calls.Load())
	}
}
