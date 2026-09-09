package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/expect"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file carries the probe source of SDD section 6.5 from the probe to the
// evaluator. The graphqlErrors default belongs to the graphql source, so the
// source has to travel with the response rather than be guessed from the body.

// jsonServer answers every request with the given body and status 200.
func jsonServer(t *testing.T, body map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestExecute_GraphQLProbeWithErrors_FailsOnTheDefault holds the load-bearing
// default of SDD section 6.5 through the whole step, not just the evaluator.
func TestExecute_GraphQLProbeWithErrors_FailsOnTheDefault(t *testing.T) {
	srv := jsonServer(t, map[string]any{
		"data":   nil,
		"errors": []any{map[string]any{"message": "field pe_tasks not found"}},
	})

	rc := &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			App: &config.AppTarget{URL: srv.URL, GraphQLPath: "/graphql"},
		}},
		HTTPClient: srv.Client(),
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}

	step := &scenario.Step{
		Name:    "read the tasks",
		Type:    "app/graphql",
		GraphQL: &scenario.GraphQLSource{Query: "query { pe_tasks { status } }"},
		Expect:  &scenario.Expect{Status: 200},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Pass {
		t.Fatal("a GraphQL error response passed a status 200 expectation")
	}
	if !strings.Contains(result.Expect.Message, "graphqlErrors") {
		t.Errorf("message %q does not name the GraphQL errors", result.Expect.Message)
	}
}

// TestExecute_HTTPProbeWithADataKey_SkipsTheGraphQLDefault keeps a REST
// response free of a check the expectation table scopes to graphql.
func TestExecute_HTTPProbeWithADataKey_SkipsTheGraphQLDefault(t *testing.T) {
	srv := jsonServer(t, map[string]any{
		"data":   map[string]any{"articleId": 515098},
		"errors": []any{map[string]any{"message": "the REST API reports its own errors list"}},
	})

	rc := &RunContext{
		Config:     &config.CitadelConfig{Targets: config.TargetsConfig{App: &config.AppTarget{URL: srv.URL}}},
		HTTPClient: srv.Client(),
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}

	step := &scenario.Step{
		Name:   "read the article",
		Type:   "app/http",
		HTTP:   &scenario.HTTPSource{Method: http.MethodGet, Path: "/articles/515098"},
		Expect: &scenario.Expect{Status: 200},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("a REST response failed a GraphQL error check it never asked for: %s", result.Expect.Message)
	}
}

// TestExecuteProbe_EverySource_NamesItself keeps the source on the probe
// result, which is where the evaluator reads it from.
func TestExecuteProbe_EverySource_NamesItself(t *testing.T) {
	srv := jsonServer(t, map[string]any{"ok": true})
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	defer listSrv.Close()

	// The journal source reads the criteria query of the WireMock admin API, so
	// its stub answers in the shape that POST /__admin/requests/find returns.
	journalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"requests":[],"requestJournalDisabled":false}`))
	}))
	defer journalSrv.Close()

	rc := &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			App:          &config.AppTarget{URL: srv.URL, GraphQLPath: "/graphql"},
			EventCapture: &config.EventCaptureTarget{URL: listSrv.URL},
			WireMock:     &config.WireMockTarget{AdminURL: journalSrv.URL},
		}},
		HTTPClient: srv.Client(),
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}

	cases := []struct {
		name string
		step *scenario.Step
		want string
	}{
		{
			name: "graphql",
			step: &scenario.Step{Name: "g", GraphQL: &scenario.GraphQLSource{Query: "query { ok }"}},
			want: expect.SourceGraphQL,
		},
		{
			name: "http",
			step: &scenario.Step{Name: "h", HTTP: &scenario.HTTPSource{Path: "/ok"}},
			want: expect.SourceHTTP,
		},
		{
			name: "captured",
			step: &scenario.Step{Name: "c", Captured: &scenario.CapturedQuery{Exchange: "public.a.v1"}},
			want: expect.SourceCaptured,
		},
		{
			name: "journal",
			step: &scenario.Step{Name: "j", Journal: &scenario.JournalQuery{}},
			want: expect.SourceJournal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ExecuteProbe(context.Background(), tc.step, rc)
			if err != nil {
				t.Fatalf("ExecuteProbe: %v", err)
			}
			if result.Source != tc.want {
				t.Errorf("source = %q, want %q", result.Source, tc.want)
			}
		})
	}
}
