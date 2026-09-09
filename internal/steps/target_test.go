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

// This file locks down the target of a probe in SDD section 6.5. These tests
// began as a proof that a probe always went to targets.app whatever target: the
// step named, which made the field a lie and check 6 of SDD section 13 vacuous
// on the runtime side.

// recordingServer answers with an empty JSON object and records each request.
type recordingServer struct {
	server *httptest.Server
	hits   int
	auth   string
	path   string
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rec := &recordingServer{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.hits++
		rec.auth = r.Header.Get("Authorization")
		rec.path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

// TestExecuteProbe_StepNamingAnotherTarget_ReachesThatTarget holds target: as
// the destination of a probe, which SDD section 6.5 shows it to be.
func TestExecuteProbe_StepNamingAnotherTarget_ReachesThatTarget(t *testing.T) {
	cases := []struct {
		name string
		step *scenario.Step
	}{
		{
			name: "http source",
			step: &scenario.Step{
				Name:   "read the trigger status",
				Type:   "probe",
				Target: "event-trigger",
				HTTP:   &scenario.HTTPSource{Method: http.MethodGet, Path: "/health"},
			},
		},
		{
			name: "graphql source",
			step: &scenario.Step{
				Name:    "query the trigger",
				Type:    "probe",
				Target:  "event-trigger",
				GraphQL: &scenario.GraphQLSource{Query: "query { ok }"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newRecordingServer(t)
			other := newRecordingServer(t)

			rc := &RunContext{
				Config: &config.CitadelConfig{Targets: config.TargetsConfig{
					App:          &config.AppTarget{URL: app.server.URL},
					EventTrigger: &config.EventTriggerTarget{URL: other.server.URL},
				}},
				HTTPClient: other.server.Client(),
				Variables:  map[string]any{},
				Bindings:   map[string]any{},
			}

			result, err := ExecuteProbe(context.Background(), tc.step, rc)
			if err != nil {
				t.Fatalf("ExecuteProbe: %v", err)
			}
			if result.Err != nil {
				t.Fatalf("the probe could not reach its target: %v", result.Err)
			}
			if other.hits != 1 {
				t.Errorf("the named target got %d requests, want 1", other.hits)
			}
			if app.hits != 0 {
				t.Errorf("the app target got %d requests, want none", app.hits)
			}
		})
	}
}

// TestExecuteProbe_NoTarget_ReachesTheApp keeps the documented default, so a
// step that names no target behaves as it always has.
func TestExecuteProbe_NoTarget_ReachesTheApp(t *testing.T) {
	app := newRecordingServer(t)

	rc := &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			App: &config.AppTarget{URL: app.server.URL},
		}},
		HTTPClient: app.server.Client(),
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}

	step := &scenario.Step{
		Name: "read the article",
		Type: "probe",
		HTTP: &scenario.HTTPSource{Method: http.MethodGet, Path: "/articles/1"},
	}

	if _, err := ExecuteProbe(context.Background(), step, rc); err != nil {
		t.Fatalf("ExecuteProbe: %v", err)
	}
	if app.hits != 1 {
		t.Errorf("the app target got %d requests, want 1", app.hits)
	}
}

// TestExecuteProbe_UnresolvableTarget_IsAnErrorThatNamesIt keeps a probe from
// silently going somewhere the scenario did not ask for.
func TestExecuteProbe_UnresolvableTarget_IsAnErrorThatNamesIt(t *testing.T) {
	cases := []struct {
		name    string
		targets config.TargetsConfig
		step    *scenario.Step
	}{
		{
			name:    "a target no config holds",
			targets: config.TargetsConfig{App: &config.AppTarget{URL: "http://127.0.0.1:1"}},
			step: &scenario.Step{
				Name:   "probe an unknown target",
				Target: "orchestration-engine",
				HTTP:   &scenario.HTTPSource{Path: "/health"},
			},
		},
		{
			name:    "a target the config holds with no address",
			targets: config.TargetsConfig{EventTrigger: &config.EventTriggerTarget{}},
			step: &scenario.Step{
				Name:   "probe a target with no address",
				Target: "event-trigger",
				HTTP:   &scenario.HTTPSource{Path: "/health"},
			},
		},
		{
			name:    "a graphql probe of an unknown target",
			targets: config.TargetsConfig{App: &config.AppTarget{URL: "http://127.0.0.1:1"}},
			step: &scenario.Step{
				Name:    "query an unknown target",
				Target:  "orchestration-engine",
				GraphQL: &scenario.GraphQLSource{Query: "query { ok }"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := &RunContext{
				Config:     &config.CitadelConfig{Targets: tc.targets},
				HTTPClient: http.DefaultClient,
				Variables:  map[string]any{},
				Bindings:   map[string]any{},
			}

			_, err := ExecuteProbe(context.Background(), tc.step, rc)
			if err == nil {
				t.Fatal("the probe ran against a target it could not resolve")
			}
			if !strings.Contains(err.Error(), tc.step.Target) {
				t.Errorf("error %q does not name the target %q", err.Error(), tc.step.Target)
			}
		})
	}
}

// TestExecuteProbe_NonAppTarget_SendsNoBearerToken keeps the token of SDD
// section 9 with the target that asks for it.
func TestExecuteProbe_NonAppTarget_SendsNoBearerToken(t *testing.T) {
	app := newRecordingServer(t)
	other := newRecordingServer(t)

	rc := &RunContext{
		Config: &config.CitadelConfig{Targets: config.TargetsConfig{
			App:          &config.AppTarget{URL: app.server.URL, Auth: "app-client"},
			EventTrigger: &config.EventTriggerTarget{URL: other.server.URL},
		}},
		HTTPClient:  other.server.Client(),
		BearerToken: "a-token",
		Variables:   map[string]any{},
		Bindings:    map[string]any{},
	}

	step := &scenario.Step{
		Name:   "read the trigger status",
		Target: "event-trigger",
		HTTP:   &scenario.HTTPSource{Method: http.MethodGet, Path: "/health"},
	}
	if _, err := ExecuteProbe(context.Background(), step, rc); err != nil {
		t.Fatalf("ExecuteProbe: %v", err)
	}
	if other.auth != "" {
		t.Errorf("the named target got the header %q, want none", other.auth)
	}

	appStep := &scenario.Step{
		Name: "read the article",
		HTTP: &scenario.HTTPSource{Method: http.MethodGet, Path: "/articles/1"},
	}
	rc.HTTPClient = app.server.Client()
	if _, err := ExecuteProbe(context.Background(), appStep, rc); err != nil {
		t.Fatalf("ExecuteProbe against the app: %v", err)
	}
	if app.auth != "Bearer a-token" {
		t.Errorf("the app target got the header %q, want the bearer token", app.auth)
	}
}
