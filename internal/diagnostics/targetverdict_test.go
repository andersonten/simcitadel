package diagnostics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// This file holds the target verdict rules of SDD section 12.3, so every
// doctor check reports a real reachability result rather than a skip.

// TestCheckTargets_UpstreamShadow_IsProbedWithSelectOne holds the row of SDD
// section 12.3 that reads "Every target reachable | Which one, and the address
// tried", and the preflight row of section 4.10 that reads "app-db,
// upstream-shadow | SELECT 1". A database target needs its own probe, because
// no other doctor check opens the shadow.
func TestCheckTargets_UpstreamShadow_IsProbedWithSelectOne(t *testing.T) {
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			UpstreamShadow: &config.DatabaseTarget{
				Driver: "postgres",
				DSN:    "postgres://citadel:citadel@127.0.0.1:1/upstream?sslmode=disable",
			},
		},
	}

	results := checkTargets(context.Background(), cfg)
	for _, r := range results {
		t.Logf("check %s: %s — %s", r.Name, r.Status, r.Message)
	}

	var probe *CheckResult
	for i, r := range results {
		if strings.Contains(r.Name, "upstream-shadow") {
			probe = &results[i]
		}
	}
	if probe == nil {
		t.Fatalf("no check names upstream-shadow, so doctor never proves the target is reachable")
	}
	if probe.Status != statusFail {
		t.Errorf("check %s reports %q for a database on a closed port, want %q", probe.Name, probe.Status, statusFail)
	}
	if !strings.Contains(probe.Message, "127.0.0.1:1") {
		t.Errorf("message %q does not name the address that doctor tried", probe.Message)
	}
	if strings.Contains(probe.Message, "citadel:citadel@") {
		t.Errorf("message %q carries the password of the DSN", probe.Message)
	}
	if probe.Remediation == "" {
		t.Error("remediation is empty on a failure")
	}
}

// TestRunDoctor_SQLServerTarget_ProducesAVerdictForBothAgentChecks holds SDD
// section 10.2: "SQL Server exists behind the cdc-sqlserver profile for teams
// whose application database is SQL Server, pinned to 2025-CU1-ubuntu-24.04,
// with the Agent enabled and the capture-job poll interval reduced". Section
// 12.3 gives each of those two facts its own doctor row. Each check must read
// the configuration and produce a real verdict for a SQL Server target.
func TestRunDoctor_SQLServerTarget_ProducesAVerdictForBothAgentChecks(t *testing.T) {
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			AppDB: &config.DatabaseTarget{
				Driver: "sqlserver",
				DSN:    "sqlserver://sa:Citadel!Passw0rd@127.0.0.1:1433?database=citadel",
			},
		},
	}

	for _, check := range []struct {
		name string
		fn   func(context.Context, *config.CitadelConfig) CheckResult
	}{
		{"sql-agent-running", checkSQLAgent},
		{"capture-job-poll-interval", checkCaptureJobPollInterval},
	} {
		result := check.fn(context.Background(), cfg)
		t.Logf("check %s: %s — %s", result.Name, result.Status, result.Message)
		if result.Status == statusSkip {
			t.Errorf("check %s returns %q although the app database driver is sqlserver",
				check.name, result.Status)
		}
		if result.Status == statusFail && result.Remediation == "" {
			t.Errorf("check %s failed with no remediation sentence, and section 12.3 puts the entire value there", check.name)
		}
		if strings.Contains(result.Message, "Citadel!Passw0rd") {
			t.Errorf("check %s carries the password of the DSN in %q", check.name, result.Message)
		}
	}
}

// TestRunDoctor_NoSQLServerTarget_SkipsBothAgentChecks holds the other half of
// the same two rows of SDD section 12.3. A Postgres application database means
// neither row applies, so each check reports a skip.
func TestRunDoctor_NoSQLServerTarget_SkipsBothAgentChecks(t *testing.T) {
	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			AppDB: &config.DatabaseTarget{
				Driver: "postgres",
				DSN:    "postgres://citadel:citadel@127.0.0.1:1/citadel?sslmode=disable",
			},
		},
	}

	for _, check := range []struct {
		name string
		fn   func(context.Context, *config.CitadelConfig) CheckResult
	}{
		{"sql-agent-running", checkSQLAgent},
		{"capture-job-poll-interval", checkCaptureJobPollInterval},
	} {
		result := check.fn(context.Background(), cfg)
		if result.Status != statusSkip {
			t.Errorf("check %s returns %q for a Postgres application database, want %q, message %q",
				check.name, result.Status, statusSkip, result.Message)
		}
	}
}

// TestCheckSpyQueues_CaptureServiceBoundToNothing_ReportsAFailure holds the row
// of SDD section 12.3 that reads "Spy queues bound | Which exchange is missing,
// and that the app may still be starting". The check holds every exchange of
// capture.exchanges to the bound state, and the exchange list that the capture
// service reports proves nothing on its own. The default profile of
// deploy/compose/docker-compose.yml sets EVENT_CAPTURE_EXCHANGES to the empty
// string, which is exactly this state.
func TestCheckSpyQueues_CaptureServiceBoundToNothing_ReportsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","exchanges":{}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			EventCapture: &config.EventCaptureTarget{URL: srv.URL},
		},
		Capture: config.CaptureConfig{
			Exchanges: []config.CaptureExchange{
				{Name: "public.orchestration-engine.Manuscript.v1", Declare: "own"},
				{Name: "public.orchestration-engine.Task.v1", Declare: "own"},
			},
		},
	}

	result := checkSpyQueues(context.Background(), cfg)
	t.Logf("check %s: %s — %s", result.Name, result.Status, result.Message)

	if result.Status != statusFail {
		t.Fatalf("the check reports %q although the configuration names %d exchanges and the service binds none",
			result.Status, len(cfg.Capture.Exchanges))
	}
	for _, exchange := range cfg.Capture.Exchanges {
		if !strings.Contains(result.Message, exchange.Name) {
			t.Errorf("message %q does not name the missing exchange %s", result.Message, exchange.Name)
		}
	}
	if !strings.Contains(result.Message, "start-up") {
		t.Errorf("message %q does not say the app can still be in start-up", result.Message)
	}
	if result.Remediation == "" {
		t.Error("remediation is empty on a failure")
	}
}

// TestCheckSpyQueues_EveryConfiguredExchangeBound_Passes covers the other half
// of the same row. A report that holds every configured exchange in the bound
// state is the one state that passes.
func TestCheckSpyQueues_EveryConfiguredExchangeBound_Passes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","exchanges":{"public.orchestration-engine.Manuscript.v1":"bound"}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &config.CitadelConfig{
		Targets: config.TargetsConfig{
			EventCapture: &config.EventCaptureTarget{URL: srv.URL},
		},
		Capture: config.CaptureConfig{
			Exchanges: []config.CaptureExchange{
				{Name: "public.orchestration-engine.Manuscript.v1", Declare: "own"},
			},
		},
	}

	result := checkSpyQueues(context.Background(), cfg)

	if result.Status != statusPass {
		t.Errorf("status = %q, want %q, message %q", result.Status, statusPass, result.Message)
	}
}

// TestProbeTarget_ServiceUnderError_CountsAsUnreachable records the reading
// that doctor takes for the reachability row of SDD section 12.3. The SDD does
// not define reachable, so doctor reads it as the narrower state: a target that
// answers 503 is up and broken, and a developer cannot run a scenario against
// it. The app readiness check of the same table reads the status code too, so
// the two siblings agree.
func TestProbeTarget_ServiceUnderError_CountsAsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	result := probeTarget(context.Background(), "wiremock", srv.URL)
	t.Logf("check %s: %s — %s", result.Name, result.Status, result.Message)

	if result.Status != statusFail {
		t.Errorf("a target that answered 503 reports %q, want %q", result.Status, statusFail)
	}
	if !strings.Contains(result.Message, "503") {
		t.Errorf("message %q does not name the status code", result.Message)
	}
	if result.Remediation == "" {
		t.Error("remediation is empty on a failure")
	}
}

// TestProbeTarget_ClientErrorFromAnAdminRoot_CountsAsReachable holds the other
// side of the same decision. The bare root of an admin API answers 401 or 404
// while the service is healthy, so only a 5xx answer is a failure. Only the
// readiness path of section 12.3 carries a 2xx contract.
func TestProbeTarget_ClientErrorFromAnAdminRoot_CountsAsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	result := probeTarget(context.Background(), "rabbitmq-management", srv.URL)

	if result.Status != statusPass {
		t.Errorf("status = %q, want %q, message %q", result.Status, statusPass, result.Message)
	}
}
