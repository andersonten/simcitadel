package steps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// journalEntry is one request of the WireMock journal, in the shape that
// POST /__admin/requests/find answers with.
type journalEntry struct {
	Method string
	URL    string
}

// fakeJournal starts a server that answers the WireMock criteria query of
// POST /__admin/requests/find over a fixed journal. It matches method in upper
// case and it matches urlPath against the path of the entry, which is how
// WireMock 3.12.1 matches. journal_container_test.go proves that match against
// the pinned image.
//
// fakeJournal returns the patterns it received, so a test can prove that the
// query of the step reached the admin API.
func fakeJournal(t *testing.T, entries ...journalEntry) (*RunContext, *[]map[string]string) {
	t.Helper()

	var patterns []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/__admin/requests/find" {
			t.Errorf("journal request: got %s %s, want POST /__admin/requests/find", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var pattern map[string]string
		if err := json.NewDecoder(r.Body).Decode(&pattern); err != nil {
			t.Errorf("decode the request pattern: %v", err)
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		patterns = append(patterns, pattern)

		matches := []map[string]string{}
		for _, e := range entries {
			if journalEntryMatches(e, pattern) {
				matches = append(matches, map[string]string{"method": e.Method, "url": e.URL})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"requests":               matches,
			"requestJournalDisabled": false,
		})
	}))
	t.Cleanup(srv.Close)

	return journalRunContext(srv.URL, srv.Client()), &patterns
}

// journalEntryMatches reports whether one journal entry matches a WireMock
// request pattern of method and urlPath.
func journalEntryMatches(e journalEntry, pattern map[string]string) bool {
	if m, ok := pattern["method"]; ok && m != e.Method {
		return false
	}
	if p, ok := pattern["urlPath"]; ok {
		path := e.URL
		if i := strings.Index(path, "?"); i >= 0 {
			path = path[:i]
		}
		if p != path {
			return false
		}
	}
	return true
}

// journalRunContext returns a run context whose WireMock target is one address.
func journalRunContext(baseURL string, client *http.Client) *RunContext {
	return &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				WireMock: &config.WireMockTarget{URL: baseURL, AdminURL: baseURL + "/__admin"},
			},
		},
		RunID:      "run-journal",
		HTTPClient: client,
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}
}

// journalStep returns a probe step that reads the journal with one query and
// one expectation.
func journalStep(name string, q *scenario.JournalQuery, exp *scenario.Expect) *scenario.Step {
	return &scenario.Step{Name: name, Type: "probe", Journal: q, Expect: exp}
}

func intOf(n int) *int { return &n }

// A journal entry with another method is not a match. The step asks for a POST,
// so a GET to the same path must not satisfy it.
func TestProbeJournal_MethodDiffers_DoesNotMatch(t *testing.T) {
	rc, patterns := fakeJournal(t, journalEntry{Method: "GET", URL: "/graphql"})
	step := journalStep("the app posted the mutation",
		&scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		&scenario.Expect{CountAtLeast: intOf(1)})

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if result.Pass {
		t.Errorf("countAtLeast: 1 passed although the journal holds only a GET")
	}
	if got := (*patterns)[0]["method"]; got != "POST" {
		t.Errorf("request pattern method: got %q, want %q", got, "POST")
	}
}

// A journal entry with another path is not a match. The step asks for
// /graphql, so a REST call must not satisfy it.
func TestProbeJournal_URLPathDiffers_DoesNotMatch(t *testing.T) {
	rc, patterns := fakeJournal(t, journalEntry{Method: "POST", URL: "/rest/articles"})
	step := journalStep("the app posted the mutation",
		&scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		&scenario.Expect{CountAtLeast: intOf(1)})

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if result.Pass {
		t.Errorf("countAtLeast: 1 passed although the journal holds no /graphql request")
	}
	if got := (*patterns)[0]["urlPath"]; got != "/graphql" {
		t.Errorf("request pattern urlPath: got %q, want %q", got, "/graphql")
	}
}

// countAtLeast counts the matches and nothing else. A journal that holds one
// match and two other requests does not satisfy countAtLeast: 2.
func TestProbeJournal_CountAtLeastAboveTheMatchCount_Fails(t *testing.T) {
	rc, _ := fakeJournal(t,
		journalEntry{Method: "POST", URL: "/graphql"},
		journalEntry{Method: "GET", URL: "/graphql"},
		journalEntry{Method: "POST", URL: "/rest/articles"})
	step := journalStep("the app posted the mutation twice",
		&scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		&scenario.Expect{CountAtLeast: intOf(2)})

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if result.Pass {
		t.Errorf("countAtLeast: 2 passed although only one request matches")
	}
	if want := "countAtLeast: expected at least 2, got 1"; result.Expect.Message != want {
		t.Errorf("message: got %q, want %q", result.Expect.Message, want)
	}
}

// count reports the size of the filtered set. Two matches out of four requests
// satisfy count: 2 and nothing else.
func TestProbeJournal_Count_EqualsTheMatchCount(t *testing.T) {
	entries := []journalEntry{
		{Method: "POST", URL: "/graphql"},
		{Method: "POST", URL: "/graphql?trace=1"},
		{Method: "GET", URL: "/graphql"},
		{Method: "POST", URL: "/rest/articles"},
	}
	rc, _ := fakeJournal(t, entries...)

	pass, err := Execute(context.Background(),
		journalStep("two mutations", &scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
			&scenario.Expect{Count: intOf(2)}), rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if !pass.Pass {
		t.Errorf("count: 2 failed against two matching requests: %s", pass.Expect.Message)
	}

	fail, err := Execute(context.Background(),
		journalStep("four mutations", &scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
			&scenario.Expect{Count: intOf(4)}), rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if fail.Pass {
		t.Errorf("count: 4 passed although only two requests match")
	}
}

// A query string does not defeat the match. SDD section 5.1 asks for urlPath
// rather than url for that reason.
func TestProbeJournal_QueryStringOnTheEntry_StillMatchesURLPath(t *testing.T) {
	rc, _ := fakeJournal(t, journalEntry{Method: "POST", URL: "/graphql?trace=1"})
	step := journalStep("the app posted the mutation",
		&scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		&scenario.Expect{Count: intOf(1)})

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if !result.Pass {
		t.Errorf("count: 1 failed against a request that carries a query string: %s", result.Expect.Message)
	}
}

// A longer path is another path. urlPath matches the whole path and not a
// prefix of it.
func TestProbeJournal_LongerPathOnTheEntry_DoesNotMatch(t *testing.T) {
	rc, _ := fakeJournal(t, journalEntry{Method: "POST", URL: "/graphql/extra"})
	step := journalStep("the app posted the mutation",
		&scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		&scenario.Expect{CountAtLeast: intOf(1)})

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if result.Pass {
		t.Errorf("countAtLeast: 1 passed although the journal holds only /graphql/extra")
	}
}

// A lower case method in the scenario reaches WireMock in upper case, because
// WireMock matches an upper case method only.
func TestProbeJournal_LowerCaseMethod_ReachesWireMockInUpperCase(t *testing.T) {
	rc, patterns := fakeJournal(t, journalEntry{Method: "POST", URL: "/graphql"})
	step := journalStep("the app posted the mutation",
		&scenario.JournalQuery{Method: "post", URLPath: "/graphql"},
		&scenario.Expect{Count: intOf(1)})

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if got := (*patterns)[0]["method"]; got != "POST" {
		t.Errorf("request pattern method: got %q, want %q", got, "POST")
	}
	if !result.Pass {
		t.Errorf("count: 1 failed for a lower case method: %s", result.Expect.Message)
	}
}

// A query with no filter reads the whole journal, so the pattern carries no
// field and the count is the size of the journal.
func TestProbeJournal_EmptyQuery_ReadsTheWholeJournal(t *testing.T) {
	rc, patterns := fakeJournal(t,
		journalEntry{Method: "POST", URL: "/graphql"},
		journalEntry{Method: "GET", URL: "/favicon.ico"})
	step := journalStep("every request", &scenario.JournalQuery{}, &scenario.Expect{Count: intOf(2)})

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("execute returned an error: %v", err)
	}
	if !result.Pass {
		t.Errorf("count: 2 failed against a journal of two requests: %s", result.Expect.Message)
	}
	if len((*patterns)[0]) != 0 {
		t.Errorf("request pattern: got %v, want an empty pattern", (*patterns)[0])
	}
}

// A journal that is off answers with an empty match list. That is an
// environment fault and not a count of zero, so the probe returns an error.
func TestProbeJournal_JournalOff_ReturnsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"requests":[],"requestJournalDisabled":true}`))
	}))
	t.Cleanup(srv.Close)

	rc := journalRunContext(srv.URL, srv.Client())
	step := journalStep("the app posted the mutation",
		&scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		&scenario.Expect{Count: intOf(0)})

	_, err := ExecuteProbe(context.Background(), step, rc)
	if err == nil {
		t.Fatal("probe journal returned no error although the journal is off")
	}
	if !strings.Contains(err.Error(), "journal is off") {
		t.Errorf("error: got %q, want a report that the journal is off", err.Error())
	}
}

// An admin API that rejects the criteria query proves nothing, so the probe
// returns an error that names the status.
func TestProbeJournal_AdminAPIRejectsTheQuery_ReturnsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"errors":[{"title":"Error parsing JSON"}]}`))
	}))
	t.Cleanup(srv.Close)

	rc := journalRunContext(srv.URL, srv.Client())
	step := journalStep("the app posted the mutation",
		&scenario.JournalQuery{Method: "POST", URLPath: "/graphql"},
		&scenario.Expect{Count: intOf(1)})

	_, err := ExecuteProbe(context.Background(), step, rc)
	if err == nil {
		t.Fatal("probe journal returned no error although the admin API answered 422")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("error: got %q, want the status in the message", err.Error())
	}
}
