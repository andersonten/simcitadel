package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// TestCheckDebeziumConnector_HealthDoesNotReportUP_DoesNotReportRunning holds
// the Debezium row of SDD section 12.3 against the condition of section 4.10:
// the health endpoint must report UP and the connector state must be RUNNING.
// A doctor that reads one half of that conjunction reports an all-clear for a
// Debezium that preflight rejects at exit 3.
func TestCheckDebeziumConnector_HealthDoesNotReportUP_DoesNotReportRunning(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantText []string
	}{
		{
			name:   "overall health is DOWN while the connector check is UP",
			status: http.StatusServiceUnavailable,
			body:   `{"status":"DOWN","checks":[{"name":"debezium","status":"UP"},{"name":"SmallRye Reactive Messaging - liveness check","status":"DOWN"}]}`,
			// The failure must quote the verdict that Debezium reported.
			wantText: []string{"DOWN"},
		},
		{
			name:   "the connector check data reports a failed state",
			status: http.StatusOK,
			body:   `{"status":"UP","checks":[{"name":"debezium","status":"UP","data":{"state":"FAILED","lastError":"org.postgresql.util.PSQLException: ERROR: replication slot \"pe_outbox_slot\" is active"}}]}`,
			wantText: []string{
				"FAILED",
				`replication slot "pe_outbox_slot" is active`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			got := checkDebeziumConnector(context.Background(), debeziumConfig(srv.URL))

			if got.Status != statusFail {
				t.Errorf("status = %q, want fail. Message: %q", got.Status, got.Message)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q does not quote %q verbatim", got.Message, want)
				}
			}
			if got.Status == statusFail && got.Remediation == "" {
				t.Error("a failure carries no remediation sentence")
			}
		})
	}
}

// brokerConfig builds a configuration whose broker credentials live in the AMQP
// URL, which is the form that SDD section 5.1 writes.
func brokerConfig(managementURL string) *config.CitadelConfig {
	return &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			RabbitMQ: &config.RabbitMQTarget{
				AMQPURL:       "amqp://citadel:citadel-secret@127.0.0.1:5672/",
				ManagementURL: managementURL,
			},
		},
	}
}

// authenticatedManagementAPI serves the RabbitMQ management API the way a real
// broker serves it: every request without the broker credentials answers 401.
func authenticatedManagementAPI(t *testing.T, policies, queues, exchanges string) *httptest.Server {
	t.Helper()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "citadel" || password != "citadel-secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="RabbitMQ Management"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/policies":
			fmt.Fprint(w, policies)
		case "/api/queues":
			fmt.Fprint(w, queues)
		case "/api/exchanges":
			fmt.Fprint(w, exchanges)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestCheckAlternateExchange_BrokerNeedsCredentials_ReadsThemFromTheAMQPURL
// holds the alternate-exchange row of SDD section 12.3. The management API of a
// real broker answers 401 without credentials, and section 5.1 keeps those
// credentials in the AMQP URL, so a check that sends none can never pass and
// its remediation tells the developer to apply a policy that already exists.
func TestCheckAlternateExchange_BrokerNeedsCredentials_ReadsThemFromTheAMQPURL(t *testing.T) {
	srv := authenticatedManagementAPI(t,
		`[{"name":"citadel-alternate-exchange","vhost":"/","pattern":"^(public|private)\\."}]`,
		`[]`, `[]`)

	got := checkAlternateExchange(context.Background(), brokerConfig(srv.URL))

	if got.Status != statusPass {
		t.Errorf("status = %q, want pass. Message: %q", got.Status, got.Message)
	}
}

// TestCheckAutoCreateRoutingKeyQueues_BrokerNeedsCredentials_ReadsThemFromTheAMQPURL
// holds the artifact-queue row of SDD section 12.3 to the same requirement.
func TestCheckAutoCreateRoutingKeyQueues_BrokerNeedsCredentials_ReadsThemFromTheAMQPURL(t *testing.T) {
	srv := authenticatedManagementAPI(t, `[]`,
		`[{"name":"public.orchestration-engine.Article.v1"},{"name":"citadel.spy.public.orchestration-engine.Article.v1"}]`,
		`[{"name":"public.orchestration-engine.Article.v1"}]`)

	got := checkAutoCreateRoutingKeyQueues(context.Background(), brokerConfig(srv.URL))

	if got.Status == statusFail {
		t.Fatalf("status = fail. Message: %q", got.Message)
	}
	if !strings.Contains(got.Message, "public.orchestration-engine.Article.v1") {
		t.Errorf("message %q does not name the artifact queue", got.Message)
	}
}

// TestCheckTargets_EventTriggerReportsADisconnectedBroker_DoesNotPass holds the
// reachability row of SDD section 12.3, which SDD section 4.1 states as
// "reachable and correctly set up". The event trigger answers 404 at its root
// and 503 at /health while its broker connection is down, so a probe of the
// root reports an all-clear for a trigger that can publish nothing.
func TestCheckTargets_EventTriggerReportsADisconnectedBroker_DoesNotPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"degraded","broker":"disconnected"}`)
	}))
	defer srv.Close()

	cfg := &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			EventTrigger: &config.EventTriggerTarget{URL: srv.URL},
		},
	}

	results := checkTargets(context.Background(), cfg)

	if len(results) != 1 {
		t.Fatalf("checkTargets returned %d results, want 1: %+v", len(results), results)
	}
	got := results[0]
	if got.Status != statusFail {
		t.Errorf("status = %q, want fail. Message: %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, srv.URL+"/health") {
		t.Errorf("message %q does not name the address that doctor tried", got.Message)
	}
}

// TestCheckTargets_EventCaptureReportsAnUnboundExchange_DoesNotPass holds the
// same requirement for the capture service, which answers 503 while a spy queue
// is unbound.
func TestCheckTargets_EventCaptureReportsAnUnboundExchange_DoesNotPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"status":"degraded","exchanges":{"public.orchestration-engine.Article.v1":"pending"}}`)
	}))
	defer srv.Close()

	cfg := &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			EventCapture: &config.EventCaptureTarget{URL: srv.URL},
		},
	}

	results := checkTargets(context.Background(), cfg)

	if len(results) != 1 {
		t.Fatalf("checkTargets returned %d results, want 1: %+v", len(results), results)
	}
	if results[0].Status != statusFail {
		t.Errorf("status = %q, want fail. Message: %q", results[0].Status, results[0].Message)
	}
}

// exampleJWT is a signed JSON Web Token in the shape every bearer token of an
// OAuth2 deployment carries: three base64url segments, the first of which
// encodes a JSON object and therefore begins with eyJ.
const exampleJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"

// TestRedactString_BareBearerToken_IsReplaced holds SDD section 12.2, which
// replaces "any string matching a known bearer-token shape wherever it
// appears". A token only carries the word Bearer inside an Authorization
// header, and the key rule of the same section already covers that key, so the
// shape rule earns its place on the token that appears without the word.
func TestRedactString_BareBearerToken_IsReplaced(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "in a query string",
			input: "GET /graphql?access_token=" + exampleJWT,
		},
		{
			name:  "in a request body that a journal entry holds as a string",
			input: `{"access_token":"` + exampleJWT + `","token_type":"bearer"}`,
		},
		{
			name:  "on its own",
			input: exampleJWT,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactString(tc.input)
			if strings.Contains(got, exampleJWT) {
				t.Errorf("RedactString(%q) = %q, and the token survives", tc.input, got)
			}
			if !strings.Contains(got, redactedValue) {
				t.Errorf("RedactString(%q) = %q, and it holds no redaction marker", tc.input, got)
			}
		})
	}
}

// TestWriteBundle_JournalHoldsABareBearerToken_IsRedacted proves the shape rule
// over the file that carries it. SDD section 12.2 applies redaction to
// journal.json, and a WireMock journal entry holds the request body as one
// string, so no key of that string reaches the key rule.
func TestWriteBundle_JournalHoldsABareBearerToken_IsRedacted(t *testing.T) {
	dir := t.TempDir()

	err := WriteBundle(BundleConfig{
		BundleDir: dir,
		RunID:     "0191c3",
		WireMock: &WireMockDiag{
			Journal: map[string]any{
				"requests": []any{
					map[string]any{
						"request": map[string]any{
							"url":  "/graphql?access_token=" + exampleJWT,
							"body": `{"query":"{ pe_tasks { id } }"}`,
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "0191c3", "wiremock", "journal.json"))
	if err != nil {
		t.Fatalf("read journal.json: %v", err)
	}
	if strings.Contains(string(data), exampleJWT) {
		t.Errorf("journal.json holds the bearer token verbatim:\n%s", data)
	}
}

// TestRunDoctor_EveryFailure_CarriesARemediation holds the opening sentence of
// SDD section 12.3: a failure reports a remediation, and the value of the
// section is entirely in that text. The configuration here names every target
// at a closed port, so every check that reads an environment fails.
func TestRunDoctor_EveryFailure_CarriesARemediation(t *testing.T) {
	closed := "http://" + closedLoopbackAddress(t)

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "test"},
		Targets: config.TargetsConfig{
			WireMock:       &config.WireMockTarget{URL: closed, AdminURL: closed + "/__admin"},
			EventTrigger:   &config.EventTriggerTarget{URL: closed},
			EventCapture:   &config.EventCaptureTarget{URL: closed},
			RabbitMQ:       &config.RabbitMQTarget{AMQPURL: "amqp://citadel:citadel@127.0.0.1:5672/", ManagementURL: closed},
			App:            &config.AppTarget{URL: closed, ReadinessPath: "/health/ready", Auth: "hydra-client"},
			DebeziumOutbox: &config.DebeziumOutboxTarget{URL: closed, ConnectorName: "outbox"},
		},
		Auth: map[string]config.AuthConfig{
			"hydra-client": {TokenURL: closed + "/oauth2/token", ClientID: "citadel", ClientSecret: "secret"},
		},
		Capture: config.CaptureConfig{
			Exchanges: []config.CaptureExchange{{Name: "public.orchestration-engine.Article.v1", Declare: "own"}},
		},
	}

	for _, result := range RunDoctor(context.Background(), cfg) {
		if result.Status == statusFail && strings.TrimSpace(result.Remediation) == "" {
			t.Errorf("check %s failed with no remediation. Message: %q", result.Name, result.Message)
		}
		if result.Status != statusPass && result.Status != statusFail && result.Status != statusSkip {
			t.Errorf("check %s reports the status %q, and section 12.3 allows pass, fail and skip only", result.Name, result.Status)
		}
	}
}

// TestManagementRequest_ManagementURLCarriesItsOwnCredential_KeepsIt proves the
// credential of the management URL wins over the AMQP URL, because a developer
// who writes one there means it.
func TestManagementRequest_ManagementURLCarriesItsOwnCredential_KeepsIt(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		seen <- user + ":" + password
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	cfg := brokerConfig(strings.Replace(srv.URL, "http://", "http://explicit:explicit-secret@", 1))

	names, err := managementNames(context.Background(), cfg.Targets.RabbitMQ, "/api/queues")
	if err != nil {
		t.Fatalf("managementNames: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want none", names)
	}
	if got := <-seen; got != "explicit:explicit-secret" {
		t.Errorf("the request carried the credential %q, want the one the management URL names", got)
	}
}

// TestCheckAlternateExchange_ManagementURLHoldsAPassword_DoesNotPrintIt keeps
// the credential out of the doctor report. A doctor report reaches a chat
// window as often as a bundle does, and SDD section 12.2 treats a credential in
// a bundle as a correctness matter.
func TestCheckAlternateExchange_ManagementURLHoldsAPassword_DoesNotPrintIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := brokerConfig(strings.Replace(srv.URL, "http://", "http://citadel:top-secret-password@", 1))

	got := checkAlternateExchange(context.Background(), cfg)

	if got.Status != statusFail {
		t.Fatalf("status = %q, want fail. Message: %q", got.Status, got.Message)
	}
	if strings.Contains(got.Message, "top-secret-password") {
		t.Errorf("the message holds the password: %q", got.Message)
	}
}

// TestJSONShapes_Fixtures_AreValidJSON keeps the fixtures of this file honest.
// A malformed fixture makes a check fail for the wrong reason.
func TestJSONShapes_Fixtures_AreValidJSON(t *testing.T) {
	fixtures := []string{
		`{"status":"DOWN","checks":[{"name":"debezium","status":"UP"}]}`,
		`[{"name":"citadel-alternate-exchange","vhost":"/","pattern":"^(public|private)\\."}]`,
	}
	for _, fixture := range fixtures {
		var parsed any
		if err := json.Unmarshal([]byte(fixture), &parsed); err != nil {
			t.Errorf("fixture %s is not valid JSON: %v", fixture, err)
		}
	}
}
