package steps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// recordPath starts a server that records the path of the request it receives.
func recordPath(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// The config format of SDD 5.1 gives adminUrl with the /__admin suffix already
// present. A step must not append it a second time.
func TestExecuteSetState_AdminURLWithAdminSuffix_DoesNotDoublePrefix(t *testing.T) {
	srv, got := recordPath(t, "{}")

	rc := &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{AdminURL: srv.URL + "/__admin"},
		}},
		HTTPClient: srv.Client(),
		Bindings:   map[string]any{},
	}
	step := &scenario.Step{Name: "s", Type: "wiremock/set-state", ScenarioName: "ArticleLifecycle", State: "ArticleSubmitted"}

	if err := ExecuteSetState(context.Background(), step, rc); err != nil {
		t.Fatalf("set-state: %v", err)
	}
	if want := "/__admin/scenarios/ArticleLifecycle/state"; *got != want {
		t.Errorf("path: got %q, want %q", *got, want)
	}
}

// An adminUrl without the suffix still has to reach the admin API.
func TestExecuteSetState_AdminURLWithoutAdminSuffix_AddsAdmin(t *testing.T) {
	srv, got := recordPath(t, "{}")

	rc := &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{AdminURL: srv.URL},
		}},
		HTTPClient: srv.Client(),
		Bindings:   map[string]any{},
	}
	step := &scenario.Step{Name: "s", Type: "wiremock/set-state", ScenarioName: "ArticleLifecycle", State: "ArticleSubmitted"}

	if err := ExecuteSetState(context.Background(), step, rc); err != nil {
		t.Fatalf("set-state: %v", err)
	}
	if want := "/__admin/scenarios/ArticleLifecycle/state"; *got != want {
		t.Errorf("path: got %q, want %q", *got, want)
	}
}

func TestProbeJournal_AdminURLWithAdminSuffix_DoesNotDoublePrefix(t *testing.T) {
	srv, got := recordPath(t, `{"requests":[]}`)

	rc := &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{AdminURL: srv.URL + "/__admin"},
		}},
		HTTPClient: srv.Client(),
		Bindings:   map[string]any{},
	}
	step := &scenario.Step{Name: "j", Type: "probe", Journal: &scenario.JournalQuery{}}

	if _, err := ExecuteProbe(context.Background(), step, rc); err != nil {
		t.Fatalf("probe journal: %v", err)
	}
	// The journal probe asks WireMock for the entries that match its query, so
	// the criteria query of the admin API is the path it reaches.
	if want := "/__admin/requests/find"; *got != want {
		t.Errorf("path: got %q, want %q", *got, want)
	}
}
