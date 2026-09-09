package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file holds the journal probe of SDD section 6.5. A probe must send its
// method and its path to WireMock as a request pattern, so the journal answers
// with the matching requests alone.

// patternRunContext returns a run context whose WireMock target is the given
// address.
func patternRunContext(t *testing.T, baseURL string, client *http.Client) *RunContext {
	t.Helper()
	return &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				WireMock: &config.WireMockTarget{URL: baseURL, AdminURL: baseURL + "/__admin"},
			},
		},
		RunID:      "run-journal",
		RunStart:   time.Now().Add(-time.Hour),
		HTTPClient: client,
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}
}

// TestProbeJournal_MethodAndPath_ReachWireMockAsARequestPattern proves that the
// journal source of SDD section 6.5 sends its own query to WireMock. The step
// names POST /graphql, so the probe posts that pair as a WireMock request
// pattern and reads back only the matches. An expectation that addresses the
// WireMock wrapper object no longer resolves, because the probe answers with the
// match list.
//
// WireMock 3.12.1 ignores a method parameter and a urlPath parameter on
// GET /__admin/requests, so the pattern travels in the body of a POST to
// /__admin/requests/find.
func TestProbeJournal_MethodAndPath_ReachWireMockAsARequestPattern(t *testing.T) {
	var seen []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		var pattern map[string]any
		if err := json.NewDecoder(r.Body).Decode(&pattern); err != nil {
			t.Errorf("decode the request pattern: %v", err)
		}
		seen = append(seen, pattern)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"requests":[
			{"request":{"method":"GET","url":"/__admin/health"}},
			{"request":{"method":"GET","url":"/favicon.ico"}}
		],"meta":{"total":2}}`))
	}))
	t.Cleanup(srv.Close)

	rc := patternRunContext(t, srv.URL, srv.Client())
	step := &scenario.Step{
		Name:    "the app called the invitation mutation",
		Type:    "probe",
		Journal: &scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		Expect:  &scenario.Expect{JSONPathExists: "$.requests[0].request.method"},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}

	if len(seen) != 1 {
		t.Fatalf("expected one journal request, got %d", len(seen))
	}
	if want := "POST /__admin" + journalFindPath; paths[0] != want {
		t.Errorf("journal request: got %q, want %q", paths[0], want)
	}
	if seen[0]["method"] != "POST" || seen[0]["urlPath"] != "/graphql" {
		t.Errorf("the request pattern dropped the query: got %v, want method %q and urlPath %q",
			seen[0], step.Journal.Method, step.Journal.URLPath)
	}
	if result.Pass {
		t.Errorf("the assertion passed although WireMock recorded no POST /graphql request")
	}
}

// TestProbeJournal_CountExpectation_CountsTheMatchedRequests holds the second
// half of the same contract. SDD section 6.5 scopes count and countAtLeast to
// the journal source, so the probe answers with the match list and the length of
// that list is the count.
func TestProbeJournal_CountExpectation_CountsTheMatchedRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"requests":[{"request":{"method":"POST","url":"/graphql"}}],"meta":{"total":1}}`))
	}))
	t.Cleanup(srv.Close)

	rc := patternRunContext(t, srv.URL, srv.Client())
	one := 1
	step := &scenario.Step{
		Name:    "the app called the mutation exactly once",
		Type:    "probe",
		Journal: &scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		Expect:  &scenario.Expect{Count: &one},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if !result.Pass {
		t.Errorf("count: 1 failed against a journal that holds exactly one request: %s",
			result.Expect.Message)
	}
}
