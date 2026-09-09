package diagnostics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/db"
)

func TestRunDoctor_NoTargets_AllSkipped(t *testing.T) {
	cfg := &config.CitadelConfig{
		Version: 1,
	}

	results := RunDoctor(context.Background(), cfg)

	for _, r := range results {
		if r.Status == "fail" {
			t.Errorf("check %s: got fail, want pass or skip", r.Name)
		}
	}

	var skipCount int
	for _, r := range results {
		if r.Status == "skip" {
			skipCount++
		}
	}
	if skipCount == 0 {
		t.Error("expected at least one skipped check when no targets are configured")
	}
}

func TestRunDoctor_UnreachableTarget_ReturnsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	cfg := &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{
				URL:      srv.URL,
				AdminURL: srv.URL,
			},
		},
	}

	results := RunDoctor(context.Background(), cfg)

	var found bool
	for _, r := range results {
		if r.Name == "target-wiremock-reachable" {
			found = true
			if r.Status != "fail" {
				t.Errorf("check %s: got %s, want fail", r.Name, r.Status)
			}
			if r.Remediation == "" {
				t.Errorf("check %s: remediation is empty", r.Name)
			}
		}
	}
	if !found {
		t.Error("target-wiremock-reachable check not found in results")
	}
}

func TestRunDoctor_ReachableTarget_ReturnsPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{
				URL:      srv.URL,
				AdminURL: srv.URL,
			},
		},
	}

	results := RunDoctor(context.Background(), cfg)

	var found bool
	for _, r := range results {
		if r.Name == "target-wiremock-reachable" {
			found = true
			if r.Status != "pass" {
				t.Errorf("check %s: got %s, want pass", r.Name, r.Status)
			}
		}
	}
	if !found {
		t.Error("target-wiremock-reachable check not found in results")
	}
}

// closedLoopbackAddress returns a loopback host and port that refuses every
// connection. A refused connection is immediate and deterministic, so a check
// that reaches the network fails fast and never waits for a timeout.
func closedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close loopback listener: %v", err)
	}
	return address
}

// unreachableDSN builds a Postgres DSN that points at a closed loopback port.
func unreachableDSN(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("postgres://citadel:citadel@%s/citadel?sslmode=disable&connect_timeout=1", closedLoopbackAddress(t))
}

// debeziumConfig builds a configuration whose only target is Debezium.
func debeziumConfig(url string) *config.CitadelConfig {
	return &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			DebeziumOutbox: &config.DebeziumOutboxTarget{URL: url, ConnectorName: "outbox"},
		},
	}
}

// TestCheckDebeziumConnector_HealthUpConnectorDown_ReturnsFail holds SDD
// section 4.10. Debezium reports healthy while a connector has failed, so the
// top-level health status alone is not evidence of a running connector.
func TestCheckDebeziumConnector_HealthUpConnectorDown_ReturnsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"UP","checks":[{"name":"debezium","status":"DOWN","data":{"state":"FAILED","lastError":"org.postgresql.util.PSQLException: ERROR: replication slot \"pe_outbox_slot\" is active"}}]}`)
	}))
	defer srv.Close()

	got := checkDebeziumConnector(context.Background(), debeziumConfig(srv.URL))

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	for _, want := range []string{"DOWN", "FAILED", `replication slot "pe_outbox_slot" is active`} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message %q does not quote %q verbatim", got.Message, want)
		}
	}
}

// TestCheckDebeziumConnector_ConnectorCheckAbsent_ReturnsFail covers the
// readiness default. Quarkus answers UP as soon as the process starts, so a
// payload without the debezium check proves nothing about the connector.
func TestCheckDebeziumConnector_ConnectorCheckAbsent_ReturnsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"UP","checks":[]}`)
	}))
	defer srv.Close()

	got := checkDebeziumConnector(context.Background(), debeziumConfig(srv.URL))

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
}

// TestCheckDebeziumConnector_ConnectorUp_ReturnsPass proves the check passes
// only when the debezium connector check itself reports UP.
func TestCheckDebeziumConnector_ConnectorUp_ReturnsPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"UP","checks":[{"name":"debezium","status":"UP"}]}`)
	}))
	defer srv.Close()

	got := checkDebeziumConnector(context.Background(), debeziumConfig(srv.URL))

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "RUNNING") {
		t.Errorf("message %q does not name the RUNNING state", got.Message)
	}
}

// hydraConfig builds a configuration with one auth block for the app target.
func hydraConfig(tokenURL, clientID, clientSecret string, scopes []string) *config.CitadelConfig {
	return &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			App: &config.AppTarget{URL: "http://app.invalid", Auth: "hydra"},
		},
		Auth: map[string]config.AuthConfig{
			"hydra": {
				TokenURL:     tokenURL,
				ClientID:     clientID,
				ClientSecret: clientSecret,
				Scopes:       scopes,
			},
		},
	}
}

// TestCheckHydraToken_ValidCredentials_PassesReportingGrantedScopes holds SDD
// section 9. The check acquires a token and reports the granted scopes.
func TestCheckHydraToken_ValidCredentials_PassesReportingGrantedScopes(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", got)
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"eyJhbGciOi.stub.token","scope":"article.read article.write","expires_in":3600}`)
	}))
	defer srv.Close()

	got := checkHydraToken(context.Background(), hydraConfig(srv.URL, "citadel", "s3cret", []string{"article.read", "article.write"}))

	if posts.Load() != 1 {
		t.Errorf("token requests = %d, want 1", posts.Load())
	}
	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "article.read article.write") {
		t.Errorf("message %q does not report the granted scopes", got.Message)
	}
}

// TestCheckHydraToken_MissingCredentials_FailsWithCredentialsMissing holds the
// SDD section 12.3 requirement to separate missing from rejected credentials.
func TestCheckHydraToken_MissingCredentials_FailsWithCredentialsMissing(t *testing.T) {
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	got := checkHydraToken(context.Background(), hydraConfig(srv.URL, "", "", nil))

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(strings.ToLower(got.Message), "missing") {
		t.Errorf("message %q does not say the credentials are missing", got.Message)
	}
	if posts.Load() != 0 {
		t.Errorf("token requests = %d, want 0 when the credentials are missing", posts.Load())
	}
}

// TestCheckHydraToken_RejectedCredentials_FailsWithCredentialsRejected proves
// the check reports the Hydra error body verbatim.
func TestCheckHydraToken_RejectedCredentials_FailsWithCredentialsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client","error_description":"Client authentication failed"}`)
	}))
	defer srv.Close()

	got := checkHydraToken(context.Background(), hydraConfig(srv.URL, "citadel", "wrong", nil))

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(strings.ToLower(got.Message), "rejected") {
		t.Errorf("message %q does not say the credentials are rejected", got.Message)
	}
	if !strings.Contains(got.Message, "invalid_client") {
		t.Errorf("message %q does not quote the Hydra error verbatim", got.Message)
	}
}

// TestCheckHydraToken_HydraUnreachable_FailsWithNoHydraNoCitadel holds SDD
// section 9. Doctor states the offline consequence explicitly.
func TestCheckHydraToken_HydraUnreachable_FailsWithNoHydraNoCitadel(t *testing.T) {
	tokenURL := "http://" + closedLoopbackAddress(t) + "/oauth2/token"

	got := checkHydraToken(context.Background(), hydraConfig(tokenURL, "citadel", "s3cret", nil))

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "no Hydra, no Citadel") {
		t.Errorf("message %q does not state the offline consequence", got.Message)
	}
}

// TestCheckHydraToken_ScopeNotGranted_FailsNamingTheMissingScope holds SDD
// section 9. A token with the wrong scopes fails later and less legibly.
func TestCheckHydraToken_ScopeNotGranted_FailsNamingTheMissingScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"stub","scope":"article.read"}`)
	}))
	defer srv.Close()

	got := checkHydraToken(context.Background(), hydraConfig(srv.URL, "citadel", "s3cret", []string{"article.read", "article.write"}))

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "article.write") {
		t.Errorf("message %q does not name the scope that Hydra withheld", got.Message)
	}
}

// databaseConfig builds a configuration whose only target is the app database.
func databaseConfig(dsn string) *config.CitadelConfig {
	inbox := &config.InboxConfig{
		Table:             config.DefaultInboxTable,
		MessageIDColumn:   config.DefaultInboxMessageIDColumn,
		ProcessedAtColumn: config.DefaultInboxProcessedAtColumn,
		RetryCountColumn:  config.DefaultInboxRetryCountColumn,
		LastErrorColumn:   config.DefaultInboxLastErrorColumn,
	}
	return &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			AppDB: &config.DatabaseTarget{Driver: "postgres", DSN: dsn, Inbox: inbox},
		},
	}
}

// TestCheckInboxTable_UnreachableDatabase_DoesNotPass holds SDD section 8.4.
// The check queries the database, so it cannot pass without one.
func TestCheckInboxTable_UnreachableDatabase_DoesNotPass(t *testing.T) {
	got := checkInboxTable(context.Background(), databaseConfig(unreachableDSN(t)))

	if got.Status == "pass" {
		t.Errorf("status = pass with no database reachable, message %q", got.Message)
	}
	if got.Status == "skip" {
		t.Errorf("status = skip while the app database is configured, message %q", got.Message)
	}
}

// TestCheckWalLevel_ConfiguredDatabase_DoesNotSkip holds SDD section 12.3.
// A configured database must produce a real answer and never a skip.
func TestCheckWalLevel_ConfiguredDatabase_DoesNotSkip(t *testing.T) {
	got := checkWalLevel(context.Background(), databaseConfig(unreachableDSN(t)))

	if got.Status == "skip" {
		t.Errorf("status = skip while the app database is configured, message %q", got.Message)
	}
}

// TestCheckStaleReplicationSlot_ConfiguredDatabase_DoesNotSkip holds SDD
// section 4.9. A surviving slot means the reset did not reset anything.
func TestCheckStaleReplicationSlot_ConfiguredDatabase_DoesNotSkip(t *testing.T) {
	got := checkStaleReplicationSlot(context.Background(), databaseConfig(unreachableDSN(t)))

	if got.Status == "skip" {
		t.Errorf("status = skip while the app database is configured, message %q", got.Message)
	}
}

// TestCheckHeartbeatDisabled_ConfiguredDebezium_DoesNotSkip holds SDD section
// 12.3. A check that never fires reports coverage it does not provide.
func TestCheckHeartbeatDisabled_ConfiguredDebezium_DoesNotSkip(t *testing.T) {
	got := checkHeartbeatDisabled(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status == "skip" {
		t.Errorf("status = skip while Debezium is configured, message %q", got.Message)
	}
}

// TestCheckRoutingKeySource_ConfiguredDebezium_DoesNotSkip holds SDD section
// 12.3 for the routing key source row.
func TestCheckRoutingKeySource_ConfiguredDebezium_DoesNotSkip(t *testing.T) {
	got := checkRoutingKeySource(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status == "skip" {
		t.Errorf("status = skip while Debezium is configured, message %q", got.Message)
	}
}

// TestCheckDebeziumImageTag_ConfiguredDebezium_DoesNotSkip holds SDD section
// 12.3 for the image tag row.
func TestCheckDebeziumImageTag_ConfiguredDebezium_DoesNotSkip(t *testing.T) {
	got := checkDebeziumImageTag(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status == "skip" {
		t.Errorf("status = skip while Debezium is configured, message %q", got.Message)
	}
}

// declareCompose writes a compose file that holds one Debezium service and
// points COMPOSE_FILE at it. The fixture makes the declared configuration
// explicit, so the test never depends on the repository layout.
func declareCompose(t *testing.T, image string, environment map[string]string) {
	t.Helper()

	var builder strings.Builder
	builder.WriteString("services:\n")
	builder.WriteString("  app-db:\n")
	builder.WriteString("    image: postgres:15\n")
	builder.WriteString("  debezium:\n")
	fmt.Fprintf(&builder, "    image: %s\n", image)
	builder.WriteString("    environment:\n")
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&builder, "      %s: %q\n", key, environment[key])
	}

	path := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatalf("write the compose fixture: %v", err)
	}
	t.Setenv("COMPOSE_FILE", path)
}

// declaredDebeziumEnvironment returns the environment of SDD section 10.1.
func declaredDebeziumEnvironment() map[string]string {
	return map[string]string{
		"DEBEZIUM_SINK_TYPE":                  "rabbitmq",
		"DEBEZIUM_SOURCE_TOPIC_PREFIX":        "orchestration-engine",
		heartbeatIntervalKey:                  "0",
		routingKeySourceKey:                   "topic",
		"DEBEZIUM_SINK_RABBITMQ_AUTOCREATERO": "true",
	}
}

// TestCheckHeartbeatDisabled_NonZeroInterval_FailsNamingTheActionQuery holds
// SDD section 10.1. The action query kills the connector or pollutes the run.
func TestCheckHeartbeatDisabled_NonZeroInterval_FailsNamingTheActionQuery(t *testing.T) {
	environment := declaredDebeziumEnvironment()
	environment[heartbeatIntervalKey] = "5000"
	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag, environment)

	got := checkHeartbeatDisabled(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	for _, want := range []string{"5000", "action query"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message %q does not name %q", got.Message, want)
		}
	}
	if got.Remediation == "" {
		t.Error("remediation is empty on a failure")
	}
}

// TestCheckHeartbeatDisabled_ZeroInterval_Passes covers the required value.
func TestCheckHeartbeatDisabled_ZeroInterval_Passes(t *testing.T) {
	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag, declaredDebeziumEnvironment())

	got := checkHeartbeatDisabled(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestCheckHeartbeatDisabled_KeyAbsent_PassesOnTheDebeziumDefault covers the
// absent key. Debezium disables the heartbeat when the interval is 0, and 0 is
// the Debezium default.
func TestCheckHeartbeatDisabled_KeyAbsent_PassesOnTheDebeziumDefault(t *testing.T) {
	environment := declaredDebeziumEnvironment()
	delete(environment, heartbeatIntervalKey)
	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag, environment)

	got := checkHeartbeatDisabled(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestCheckRoutingKeySource_SourceAbsent_FailsNamingTheEmptyRoutingKey holds
// the SDD section 12.3 failure text for the routing key row.
func TestCheckRoutingKeySource_SourceAbsent_FailsNamingTheEmptyRoutingKey(t *testing.T) {
	environment := declaredDebeziumEnvironment()
	delete(environment, routingKeySourceKey)
	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag, environment)

	got := checkRoutingKeySource(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "empty routing key") {
		t.Errorf("message %q does not name the empty routing key", got.Message)
	}
}

// TestCheckRoutingKeySource_LegacyKeyPresent_FailsNamingIt holds the SDD
// section 10.1 rule that routingKeyFromTopicName stays absent.
func TestCheckRoutingKeySource_LegacyKeyPresent_FailsNamingIt(t *testing.T) {
	environment := declaredDebeziumEnvironment()
	environment[routingKeyFromTopicKey] = "true"
	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag, environment)

	got := checkRoutingKeySource(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, routingKeyFromTopicKey) {
		t.Errorf("message %q does not name %s", got.Message, routingKeyFromTopicKey)
	}
}

// TestCheckRoutingKeySource_SourceTopic_Passes covers the required value.
func TestCheckRoutingKeySource_SourceTopic_Passes(t *testing.T) {
	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag, declaredDebeziumEnvironment())

	got := checkRoutingKeySource(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestCheckDebeziumImageTag_NewerTag_FailsNamingBothVersions holds SDD section
// 10.1. A newer Debezium than production defeats the purpose of running the
// outbound pipeline for real.
func TestCheckDebeziumImageTag_NewerTag_FailsNamingBothVersions(t *testing.T) {
	declareCompose(t, "quay.io/debezium/server:3.6.1.Final", declaredDebeziumEnvironment())

	got := checkDebeziumImageTag(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	for _, want := range []string{"3.6.1.Final", requiredDebeziumImageTag, "production"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message %q does not name %q", got.Message, want)
		}
	}
}

// TestCheckDebeziumImageTag_PinnedTag_Passes covers a tag with a digest, which
// is the form that the compose file of section 11.1 uses.
func TestCheckDebeziumImageTag_PinnedTag_Passes(t *testing.T) {
	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag+"@sha256:197f5eaf72b8bfcad1f0a8cca6c663fa1acecde72aa287b7617cef912a218e18", declaredDebeziumEnvironment())

	got := checkDebeziumImageTag(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestCheckDebeziumImageTag_NoTag_Fails covers a floating image reference.
func TestCheckDebeziumImageTag_NoTag_Fails(t *testing.T) {
	declareCompose(t, "quay.io/debezium/server", declaredDebeziumEnvironment())

	got := checkDebeziumImageTag(context.Background(), debeziumConfig("http://debezium.invalid"))

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
}

// TestImageTag_References_ReturnTheTag covers the reference forms that a
// compose file holds.
func TestImageTag_References_ReturnTheTag(t *testing.T) {
	cases := []struct {
		reference string
		want      string
	}{
		{"quay.io/debezium/server:3.2.0.Final", "3.2.0.Final"},
		{"quay.io/debezium/server:3.2.0.Final@sha256:abc", "3.2.0.Final"},
		{"postgres:15", "15"},
		{"quay.io/debezium/server", ""},
		{"registry.example.com:5000/debezium/server", ""},
		{"registry.example.com:5000/debezium/server:3.2.0.Final", "3.2.0.Final"},
	}

	for _, tc := range cases {
		if got := imageTag(tc.reference); got != tc.want {
			t.Errorf("imageTag(%q) = %q, want %q", tc.reference, got, tc.want)
		}
	}
}

// TestLoadDeclaredDebezium_EnvironmentAsSequence_ReadsEveryKey covers the
// second environment form that compose accepts.
func TestLoadDeclaredDebezium_EnvironmentAsSequence_ReadsEveryKey(t *testing.T) {
	content := "services:\n" +
		"  debezium:\n" +
		"    image: quay.io/debezium/server:3.2.0.Final\n" +
		"    environment:\n" +
		"      - DEBEZIUM_SINK_TYPE=rabbitmq\n" +
		"      - " + routingKeySourceKey + "=topic\n" +
		"      - " + heartbeatIntervalKey + "=0\n"
	path := filepath.Join(t.TempDir(), "docker-compose.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write the compose fixture: %v", err)
	}
	t.Setenv("COMPOSE_FILE", path)

	declared, err := loadDeclaredDebezium()
	if err != nil {
		t.Fatalf("loadDeclaredDebezium: %v", err)
	}

	if declared.service != "debezium" {
		t.Errorf("service = %q, want debezium", declared.service)
	}
	if got := declared.environment[routingKeySourceKey]; got != "topic" {
		t.Errorf("%s = %q, want topic", routingKeySourceKey, got)
	}
	if got := declared.environment[heartbeatIntervalKey]; got != "0" {
		t.Errorf("%s = %q, want 0", heartbeatIntervalKey, got)
	}
}

// TestFindComposeFile_DeployComposeInAParentDirectory_IsFound covers the
// directory walk that runs when COMPOSE_FILE is unset.
func TestFindComposeFile_DeployComposeInAParentDirectory_IsFound(t *testing.T) {
	t.Setenv("COMPOSE_FILE", "")
	root := t.TempDir()
	composeDir := filepath.Join(root, "deploy", "compose")
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatalf("create the compose directory: %v", err)
	}
	want := filepath.Join(composeDir, "docker-compose.yml")
	if err := os.WriteFile(want, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write the compose fixture: %v", err)
	}
	nested := filepath.Join(root, "internal", "diagnostics")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("create the nested directory: %v", err)
	}
	t.Chdir(nested)

	got := findComposeFile()

	// The temporary directory can sit behind a symlink, so compare the
	// resolved paths.
	resolvedGot, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("resolve %q: %v", got, err)
	}
	resolvedWant, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("resolve %q: %v", want, err)
	}
	if resolvedGot != resolvedWant {
		t.Errorf("findComposeFile() = %q, want %q", resolvedGot, resolvedWant)
	}
}

// TestRunDoctor_RepositoryComposeFile_DebeziumDeclaredChecksPass proves the
// compose file that this repository ships satisfies section 10.1.
func TestRunDoctor_RepositoryComposeFile_DebeziumDeclaredChecksPass(t *testing.T) {
	t.Setenv("COMPOSE_FILE", filepath.Join("..", "..", "deploy", "compose", "docker-compose.yml"))
	cfg := debeziumConfig("http://debezium.invalid")

	cases := map[string]CheckResult{
		"debezium-image-tag":     checkDebeziumImageTag(context.Background(), cfg),
		"heartbeat-disabled":     checkHeartbeatDisabled(context.Background(), cfg),
		"routing-key-source-set": checkRoutingKeySource(context.Background(), cfg),
	}

	for name, got := range cases {
		if got.Status != "pass" {
			t.Errorf("check %s: status = %q, want pass, message %q", name, got.Status, got.Message)
		}
	}
}

// fakeDriverName names the driver that the database checks run against in a
// test. database/sql needs a driver and not an interface, so a second driver
// keeps the query and the comparison under test with no Postgres.
const fakeDriverName = "diagnostics-fake"

func init() {
	sql.Register(fakeDriverName, fakeDriver{})
}

// fakeResult holds the answer that the fake driver gives for one query.
type fakeResult struct {
	columns []string
	rows    [][]driver.Value
	err     error
}

var (
	fakeMu    sync.Mutex
	fakeStore = map[string]map[string]fakeResult{}
	fakeSeq   int
)

// newFakeDatabase opens a handle whose queries answer from the given map.
func newFakeDatabase(t *testing.T, answers map[string]fakeResult) *sql.DB {
	t.Helper()

	fakeMu.Lock()
	fakeSeq++
	name := fmt.Sprintf("fake-%d", fakeSeq)
	fakeStore[name] = answers
	fakeMu.Unlock()

	handle, err := sql.Open(fakeDriverName, name)
	if err != nil {
		t.Fatalf("open the fake database: %v", err)
	}
	t.Cleanup(func() {
		handle.Close()
		fakeMu.Lock()
		delete(fakeStore, name)
		fakeMu.Unlock()
	})
	return handle
}

type fakeDriver struct{}

func (fakeDriver) Open(name string) (driver.Conn, error) {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	answers, ok := fakeStore[name]
	if !ok {
		return nil, fmt.Errorf("the fake driver holds no answers for %q", name)
	}
	return &fakeConn{answers: answers}, nil
}

type fakeConn struct {
	answers map[string]fakeResult
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("the fake driver answers through QueryContext only")
}

func (c *fakeConn) Close() error { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("the fake driver runs no transaction")
}

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	answer, ok := c.answers[query]
	if !ok {
		return nil, fmt.Errorf("the fake driver received an unexpected query: %s", query)
	}
	if answer.err != nil {
		return nil, answer.err
	}
	return &fakeRows{columns: answer.columns, rows: answer.rows}, nil
}

type fakeRows struct {
	columns []string
	rows    [][]driver.Value
	next    int
}

func (r *fakeRows) Columns() []string { return r.columns }

func (r *fakeRows) Close() error { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

// TestDatabaseQueries_Text_ReadTheRealSources proves each database check reads
// the source that SDD section 12.3 names.
func TestDatabaseQueries_Text_ReadTheRealSources(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{walLevelQuery, "SHOW wal_level"},
		{replicationSlotQuery, "pg_replication_slots"},
		{inboxColumnQuery, "information_schema.columns"},
	}

	for _, tc := range cases {
		if !strings.Contains(tc.query, tc.want) {
			t.Errorf("query %q does not read %s", tc.query, tc.want)
		}
	}
}

// TestWalLevelResult_ReplicaLevel_FailsWithTheCommandOverride holds the SDD
// section 12.3 row for wal_level.
func TestWalLevelResult_ReplicaLevel_FailsWithTheCommandOverride(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		walLevelQuery: {columns: []string{"wal_level"}, rows: [][]driver.Value{{"replica"}}},
	})

	got := walLevelResult(context.Background(), handle)

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "replica") {
		t.Errorf("message %q does not name the actual level", got.Message)
	}
	if !strings.Contains(got.Remediation, `command: ["postgres", "-c", "wal_level=logical"]`) {
		t.Errorf("remediation %q does not give the command override", got.Remediation)
	}
}

// TestWalLevelResult_LogicalLevel_Passes covers the required level.
func TestWalLevelResult_LogicalLevel_Passes(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		walLevelQuery: {columns: []string{"wal_level"}, rows: [][]driver.Value{{"logical"}}},
	})

	got := walLevelResult(context.Background(), handle)

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestStaleReplicationSlotResult_InactiveSlot_FailsWithDownV holds SDD section
// 4.9. A surviving slot means the reset did not reset the environment.
func TestStaleReplicationSlotResult_InactiveSlot_FailsWithDownV(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		replicationSlotQuery: {
			columns: []string{"slot_name", "active"},
			rows:    [][]driver.Value{{"pe_outbox_slot", false}},
		},
	})

	got := staleReplicationSlotResult(context.Background(), handle)

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "pe_outbox_slot") {
		t.Errorf("message %q does not name the slot", got.Message)
	}
	if !strings.Contains(got.Remediation, "docker compose down -v") {
		t.Errorf("remediation %q does not give the down -v needed for a true reset", got.Remediation)
	}
}

// TestStaleReplicationSlotResult_ActiveSlot_Passes covers the slot of the
// Debezium that runs now. An active slot has a walsender attached, so it is
// not a leftover.
func TestStaleReplicationSlotResult_ActiveSlot_Passes(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		replicationSlotQuery: {
			columns: []string{"slot_name", "active"},
			rows:    [][]driver.Value{{"pe_outbox_slot", true}},
		},
	})

	got := staleReplicationSlotResult(context.Background(), handle)

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestStaleReplicationSlotResult_NoSlot_Passes covers a clean database.
func TestStaleReplicationSlotResult_NoSlot_Passes(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		replicationSlotQuery: {columns: []string{"slot_name", "active"}},
	})

	got := staleReplicationSlotResult(context.Background(), handle)

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// referenceInbox returns the inbox configuration with the reference defaults.
func referenceInbox() *config.InboxConfig {
	return &config.InboxConfig{
		Table:             config.DefaultInboxTable,
		MessageIDColumn:   config.DefaultInboxMessageIDColumn,
		ProcessedAtColumn: config.DefaultInboxProcessedAtColumn,
		RetryCountColumn:  config.DefaultInboxRetryCountColumn,
		LastErrorColumn:   config.DefaultInboxLastErrorColumn,
	}
}

// inboxColumnRows builds the information_schema answer for a set of columns.
func inboxColumnRows(schema string, columns ...string) fakeResult {
	rows := make([][]driver.Value, 0, len(columns))
	for _, column := range columns {
		rows = append(rows, []driver.Value{schema, column})
	}
	return fakeResult{columns: []string{"table_schema", "column_name"}, rows: rows}
}

// TestInboxTableResult_AllColumnsPresent_Passes covers a table that matches
// every configured name.
func TestInboxTableResult_AllColumnsPresent_Passes(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		inboxColumnQuery: inboxColumnRows("public", "message_id", "processed_at", "retry_count", "last_error"),
	})

	got := inboxTableResult(context.Background(), handle, referenceInbox(), db.DialectPostgres)

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestInboxTableResult_MissingTable_FailsNamingTheExpectedColumns holds SDD
// section 8.4. A wrong name otherwise surfaces as a SQL error mid-scenario.
func TestInboxTableResult_MissingTable_FailsNamingTheExpectedColumns(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		inboxColumnQuery: inboxColumnRows("public"),
	})

	got := inboxTableResult(context.Background(), handle, referenceInbox(), db.DialectPostgres)

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	for _, want := range []string{"inbox", "message_id", "processed_at", "retry_count", "last_error"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message %q does not name the expected name %q", got.Message, want)
		}
	}
}

// TestInboxTableResult_MissingColumn_FailsNamingExpectedAgainstActual proves
// the check compares every configured column and not only messageIdColumn.
func TestInboxTableResult_MissingColumn_FailsNamingExpectedAgainstActual(t *testing.T) {
	handle := newFakeDatabase(t, map[string]fakeResult{
		inboxColumnQuery: inboxColumnRows("public", "message_id", "processed_at", "attempt_count", "failure_reason"),
	})

	got := inboxTableResult(context.Background(), handle, referenceInbox(), db.DialectPostgres)

	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail, message %q", got.Status, got.Message)
	}
	for _, want := range []string{"retry_count", "last_error", "attempt_count", "failure_reason"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message %q does not set the expected names against the actual ones, %q is absent", got.Message, want)
		}
	}
}

// TestInboxTableResult_OverriddenColumnNames_Passes covers a service that
// names its columns differently, as SDD section 8.4 allows.
func TestInboxTableResult_OverriddenColumnNames_Passes(t *testing.T) {
	inbox := &config.InboxConfig{
		Table:             "messaging.inbox",
		MessageIDColumn:   "msg_id",
		ProcessedAtColumn: "handled_at",
		RetryCountColumn:  "attempts",
		LastErrorColumn:   "error_text",
	}
	handle := newFakeDatabase(t, map[string]fakeResult{
		inboxColumnQuery: {
			columns: []string{"table_schema", "column_name"},
			rows: [][]driver.Value{
				{"public", "message_id"},
				{"messaging", "msg_id"},
				{"messaging", "handled_at"},
				{"messaging", "attempts"},
				{"messaging", "error_text"},
			},
		},
	})

	got := inboxTableResult(context.Background(), handle, inbox, db.DialectPostgres)

	if got.Status != "pass" {
		t.Errorf("status = %q, want pass, message %q", got.Status, got.Message)
	}
}

// TestInboxTableResult_TableInAnotherSchema_Fails proves the schema qualifier
// of the configured table narrows the comparison.
func TestInboxTableResult_TableInAnotherSchema_Fails(t *testing.T) {
	inbox := referenceInbox()
	inbox.Table = "messaging.inbox"
	handle := newFakeDatabase(t, map[string]fakeResult{
		inboxColumnQuery: inboxColumnRows("public", "message_id", "processed_at", "retry_count", "last_error"),
	})

	got := inboxTableResult(context.Background(), handle, inbox, db.DialectPostgres)

	if got.Status != "fail" {
		t.Errorf("status = %q, want fail, message %q", got.Status, got.Message)
	}
}

// TestRunDoctor_EveryRowOfSection123_HasExactlyOneResult holds SDD section
// 12.3. Every row produces one result, and every failure carries a remediation.
func TestRunDoctor_EveryRowOfSection123_HasExactlyOneResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/policies"):
			fmt.Fprint(w, `[{"name":"citadel-alternate-exchange"}]`)
		case strings.HasSuffix(r.URL.Path, "/api/queues"), strings.HasSuffix(r.URL.Path, "/api/exchanges"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/q/health"):
			fmt.Fprint(w, `{"status":"UP","checks":[{"name":"debezium","status":"UP"}]}`)
		case strings.HasSuffix(r.URL.Path, "/health"):
			fmt.Fprint(w, `{"exchanges":{}}`)
		case strings.HasSuffix(r.URL.Path, "/oauth2/token"):
			fmt.Fprint(w, `{"access_token":"stub","scope":"article.read"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()

	declareCompose(t, "quay.io/debezium/server:"+requiredDebeziumImageTag, declaredDebeziumEnvironment())

	cfg := &config.CitadelConfig{
		Version: 1,
		Targets: config.TargetsConfig{
			WireMock:       &config.WireMockTarget{URL: srv.URL, AdminURL: srv.URL},
			EventTrigger:   &config.EventTriggerTarget{URL: srv.URL},
			EventCapture:   &config.EventCaptureTarget{URL: srv.URL},
			RabbitMQ:       &config.RabbitMQTarget{ManagementURL: srv.URL},
			App:            &config.AppTarget{URL: srv.URL, ReadinessPath: "/health", Auth: "hydra"},
			DebeziumOutbox: &config.DebeziumOutboxTarget{URL: srv.URL, ConnectorName: "outbox"},
			// An unsupported driver keeps the database checks off the
			// network and still exercises the result they report.
			AppDB: &config.DatabaseTarget{Driver: "mysql", DSN: "mysql://nowhere", Inbox: referenceInbox()},
		},
		Auth: map[string]config.AuthConfig{
			"hydra": {TokenURL: srv.URL + "/oauth2/token", ClientID: "citadel", ClientSecret: "s3cret", Scopes: []string{"article.read"}},
		},
	}

	results := RunDoctor(context.Background(), cfg)

	// One entry for each row of the SDD section 12.3 table, in table order.
	rows := []struct {
		row   string
		check string
	}{
		{"Config parses, all ${VAR} resolved", "config-valid"},
		{"Every target reachable", "target-wiremock-reachable"},
		{"Hydra token acquires", "hydra-token"},
		{"Spy queues bound", "spy-queues-bound"},
		{"Alternate-exchange policy present", "alternate-exchange-policy"},
		{"Debezium connector RUNNING", "debezium-connector-running"},
		{"Debezium image tag matches the devops template", "debezium-image-tag"},
		{"Heartbeat disabled", "heartbeat-disabled"},
		{"routingKey.source set", "routing-key-source-set"},
		{"Queues whose name equals an exchange name", "auto-create-routing-key-queues"},
		{"Inbox table and columns exist", "inbox-table-exists"},
		{"Postgres wal_level=logical", "postgres-wal-level"},
		{"No stale replication slot", "no-stale-replication-slot"},
		{"SQL Agent running", "sql-agent-running"},
		{"Capture-job poll interval applied", "capture-job-poll-interval"},
		{"App readiness endpoint responds", "app-readiness"},
	}

	for _, row := range rows {
		got := findResult(t, results, row.check)
		if got.Status != "pass" && got.Status != "fail" && got.Status != "skip" {
			t.Errorf("row %q, check %s: status = %q, want pass, fail or skip", row.row, row.check, got.Status)
		}
		if got.Message == "" {
			t.Errorf("row %q, check %s: message is empty", row.row, row.check)
		}
	}

	for _, got := range results {
		if got.Status == "fail" && got.Remediation == "" {
			t.Errorf("check %s failed with no remediation sentence, message %q", got.Name, got.Message)
		}
	}
}

// findResult returns the single result with the given name.
func findResult(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	var found []CheckResult
	for _, r := range results {
		if r.Name == name {
			found = append(found, r)
		}
	}
	if len(found) == 0 {
		t.Fatalf("check %s is absent from the doctor results", name)
	}
	if len(found) > 1 {
		t.Fatalf("check %s appears %d times, want exactly once", name, len(found))
	}
	return found[0]
}
