package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

func TestExecuteProbe_HTTPSource_MakesRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/status" {
			t.Errorf("expected /api/status, got %s", r.URL.Path)
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"status": "healthy"})
	}))
	defer srv.Close()

	step := &scenario.Step{
		Name: "http-probe",
		Type: "probe",
		HTTP: &scenario.HTTPSource{
			Method: "GET",
			Path:   "/api/status",
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

	result, err := ExecuteProbe(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Err != nil {
		t.Fatalf("unexpected probe error: %v", result.Err)
	}
	if result.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", result.StatusCode)
	}
	body, ok := result.Response.(map[string]any)
	if !ok {
		t.Fatalf("expected map response, got %T", result.Response)
	}
	if body["status"] != "healthy" {
		t.Fatalf("expected status=healthy, got %v", body["status"])
	}
}

func TestExecuteProbe_GraphQLSource_MakesRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/graphql" {
			t.Errorf("expected /graphql, got %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json content-type, got %s", ct)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["query"] != "{ users { id } }" {
			t.Errorf("unexpected query: %v", body["query"])
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"users": []any{
					map[string]any{"id": "1"},
				},
			},
		})
	}))
	defer srv.Close()

	step := &scenario.Step{
		Name: "graphql-probe",
		Type: "probe",
		GraphQL: &scenario.GraphQLSource{
			Query: "{ users { id } }",
		},
	}

	rc := &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				App: &config.AppTarget{
					URL:         srv.URL,
					GraphQLPath: "/graphql",
				},
			},
		},
		HTTPClient: srv.Client(),
		Bindings:   make(map[string]any),
	}

	result, err := ExecuteProbe(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Err != nil {
		t.Fatalf("unexpected probe error: %v", result.Err)
	}
	if result.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", result.StatusCode)
	}
}
