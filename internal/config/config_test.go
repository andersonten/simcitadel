package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `version: 1
environment:
  name: local
targets:
  wiremock:
    url: http://127.0.0.1:9090
    adminUrl: http://127.0.0.1:9090/__admin
  event-trigger:
    url: http://127.0.0.1:8081
  event-capture:
    url: http://127.0.0.1:8082
  rabbitmq:
    amqpUrl: "amqp://${TEST_AMQP_USER}:${TEST_AMQP_PASS}@127.0.0.1:5672/"
    managementUrl: http://127.0.0.1:15672
  app:
    url: http://127.0.0.1:5000
    graphqlPath: /graphql
    readinessPath: /health/ready
    auth: hydra-client
  app-db:
    driver: postgres
    dsn: "${TEST_APP_DB_DSN}"
    inbox:
      table: inbox
      messageIdColumn: message_id
  upstream-shadow:
    driver: postgres
    dsn: "${TEST_SHADOW_DB_DSN}"
  debezium-outbox:
    url: http://127.0.0.1:8083
    connectorName: reference-app-outbox
auth:
  hydra-client:
    tokenUrl: "${TEST_HYDRA_TOKEN_URL}"
    clientId: citadel
    clientSecret: "${TEST_CLIENT_SECRET}"
    scopes:
      - reference-app.read
capture:
  exchanges:
    - name: public.orchestration-engine.Article.v1
      declare: own
  ignore:
    - private.orchestration-engine.Heartbeat.v1
defaults:
  awaitTimeout: 30s
  awaitInterval: 250ms
  quietPeriod: 5s
  publishBudget: 100
  appReadyTimeout: 60s
  resetSettle: 250ms
  strictUnconsumed: false
  strictUnmatched: false
paths:
  scenarios: ./scenarios
  payloads: ./payloads
  sql: ./sql
  queries: ./queries
  wiremock: ./wiremock
kubernetes:
  context: my-cluster
  namespace: default
  forwards:
    - service: citadel-wiremock
      localPort: 9090
      remotePort: 8080
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "citadel.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return p
}

func TestLoad_ValidConfig_ParsesAllFields(t *testing.T) {
	t.Setenv("TEST_AMQP_USER", "guest")
	t.Setenv("TEST_AMQP_PASS", "guest")
	t.Setenv("TEST_APP_DB_DSN", "postgres://user:pass@127.0.0.1:5432/appdb")
	t.Setenv("TEST_SHADOW_DB_DSN", "postgres://user:pass@127.0.0.1:5432/shadow")
	t.Setenv("TEST_HYDRA_TOKEN_URL", "http://hydra:4444/oauth2/token")
	t.Setenv("TEST_CLIENT_SECRET", "secret")
	p := writeConfig(t, validConfig)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Version != 1 {
		t.Fatalf("version: got %d, want 1", cfg.Version)
	}
	if cfg.Environment.Name != "local" {
		t.Fatalf("environment.name: got %q, want %q", cfg.Environment.Name, "local")
	}
	if cfg.Targets.WireMock == nil {
		t.Fatal("targets.wiremock is nil")
	}
	if cfg.Targets.WireMock.URL != "http://127.0.0.1:9090" {
		t.Fatalf("targets.wiremock.url: got %q", cfg.Targets.WireMock.URL)
	}
	if cfg.Targets.WireMock.AdminURL != "http://127.0.0.1:9090/__admin" {
		t.Fatalf("targets.wiremock.adminUrl: got %q", cfg.Targets.WireMock.AdminURL)
	}
	if cfg.Targets.EventTrigger == nil || cfg.Targets.EventTrigger.URL != "http://127.0.0.1:8081" {
		t.Fatal("targets.event-trigger not parsed correctly")
	}
	if cfg.Targets.EventCapture == nil || cfg.Targets.EventCapture.URL != "http://127.0.0.1:8082" {
		t.Fatal("targets.event-capture not parsed correctly")
	}
	if cfg.Targets.RabbitMQ == nil {
		t.Fatal("targets.rabbitmq is nil")
	}
	if cfg.Targets.RabbitMQ.AMQPURL != "amqp://guest:guest@127.0.0.1:5672/" {
		t.Fatalf("targets.rabbitmq.amqpUrl: got %q", cfg.Targets.RabbitMQ.AMQPURL)
	}
	if cfg.Targets.App == nil {
		t.Fatal("targets.app is nil")
	}
	if cfg.Targets.App.GraphQLPath != "/graphql" {
		t.Fatalf("targets.app.graphqlPath: got %q", cfg.Targets.App.GraphQLPath)
	}
	if cfg.Targets.App.Auth != "hydra-client" {
		t.Fatalf("targets.app.auth: got %q", cfg.Targets.App.Auth)
	}
	if cfg.Targets.AppDB == nil {
		t.Fatal("targets.app-db is nil")
	}
	if cfg.Targets.AppDB.Driver != "postgres" {
		t.Fatalf("targets.app-db.driver: got %q", cfg.Targets.AppDB.Driver)
	}
	if cfg.Targets.AppDB.Inbox == nil || cfg.Targets.AppDB.Inbox.Table != "inbox" {
		t.Fatal("targets.app-db.inbox not parsed correctly")
	}
	if cfg.Targets.UpstreamShadow == nil {
		t.Fatal("targets.upstream-shadow is nil")
	}
	if cfg.Targets.DebeziumOutbox == nil || cfg.Targets.DebeziumOutbox.ConnectorName != "reference-app-outbox" {
		t.Fatal("targets.debezium-outbox not parsed correctly")
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("auth: got %d entries, want 1", len(cfg.Auth))
	}
	hydra, ok := cfg.Auth["hydra-client"]
	if !ok {
		t.Fatal("auth.hydra-client not found")
	}
	if hydra.ClientID != "citadel" {
		t.Fatalf("auth.hydra-client.clientId: got %q", hydra.ClientID)
	}
	if len(hydra.Scopes) != 1 || hydra.Scopes[0] != "reference-app.read" {
		t.Fatalf("auth.hydra-client.scopes: got %v", hydra.Scopes)
	}
	if len(cfg.Capture.Exchanges) != 1 {
		t.Fatalf("capture.exchanges: got %d, want 1", len(cfg.Capture.Exchanges))
	}
	if cfg.Capture.Exchanges[0].Name != "public.orchestration-engine.Article.v1" {
		t.Fatalf("capture.exchanges[0].name: got %q", cfg.Capture.Exchanges[0].Name)
	}
	if cfg.Capture.Exchanges[0].Declare != "own" {
		t.Fatalf("capture.exchanges[0].declare: got %q", cfg.Capture.Exchanges[0].Declare)
	}
	if len(cfg.Capture.Ignore) != 1 {
		t.Fatalf("capture.ignore: got %d, want 1", len(cfg.Capture.Ignore))
	}
	if cfg.Defaults.AwaitTimeout.Duration() != 30*time.Second {
		t.Fatalf("defaults.awaitTimeout: got %v", cfg.Defaults.AwaitTimeout.Duration())
	}
	if cfg.Defaults.AwaitInterval.Duration() != 250*time.Millisecond {
		t.Fatalf("defaults.awaitInterval: got %v", cfg.Defaults.AwaitInterval.Duration())
	}
	if cfg.Defaults.PublishBudget != 100 {
		t.Fatalf("defaults.publishBudget: got %d", cfg.Defaults.PublishBudget)
	}
	if cfg.Defaults.StrictUnconsumed {
		t.Fatal("defaults.strictUnconsumed: got true, want false")
	}
	if cfg.Paths.Scenarios != "./scenarios" {
		t.Fatalf("paths.scenarios: got %q", cfg.Paths.Scenarios)
	}
	if cfg.Kubernetes == nil {
		t.Fatal("kubernetes is nil")
	}
	if cfg.Kubernetes.Context != "my-cluster" {
		t.Fatalf("kubernetes.context: got %q", cfg.Kubernetes.Context)
	}
	if len(cfg.Kubernetes.Forwards) != 1 {
		t.Fatalf("kubernetes.forwards: got %d, want 1", len(cfg.Kubernetes.Forwards))
	}
	if cfg.Kubernetes.Forwards[0].LocalPort != 9090 {
		t.Fatalf("kubernetes.forwards[0].localPort: got %d", cfg.Kubernetes.Forwards[0].LocalPort)
	}
}

func TestLoad_EnvVarInterpolation_SubstitutesValues(t *testing.T) {
	t.Setenv("TEST_AMQP_USER", "myuser")
	t.Setenv("TEST_AMQP_PASS", "mypass")
	yaml := `version: 1
environment:
  name: local
targets:
  rabbitmq:
    amqpUrl: "amqp://${TEST_AMQP_USER}:${TEST_AMQP_PASS}@127.0.0.1:5672/"
    managementUrl: http://127.0.0.1:15672
`
	p := writeConfig(t, yaml)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "amqp://myuser:mypass@127.0.0.1:5672/"
	if cfg.Targets.RabbitMQ.AMQPURL != want {
		t.Fatalf("got %q, want %q", cfg.Targets.RabbitMQ.AMQPURL, want)
	}
}

func TestLoad_EnvVarWithDefault_UsesDefault(t *testing.T) {
	yaml := `version: 1
environment:
  name: "${CITADEL_TEST_ENV:-staging}"
`
	p := writeConfig(t, yaml)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment.Name != "staging" {
		t.Fatalf("got %q, want %q", cfg.Environment.Name, "staging")
	}
}

func TestLoad_UnsetRequiredEnvVar_ReturnsErrorNamingVar(t *testing.T) {
	yaml := `version: 1
environment:
  name: local
targets:
  rabbitmq:
    amqpUrl: "amqp://${CITADEL_MISSING_VAR}@127.0.0.1:5672/"
    managementUrl: http://127.0.0.1:15672
`
	p := writeConfig(t, yaml)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error for an unset environment variable")
	}
	if !strings.Contains(err.Error(), "CITADEL_MISSING_VAR") {
		t.Fatalf("error %q does not name the variable CITADEL_MISSING_VAR", err.Error())
	}
}

func TestLoad_UnknownTopLevelKey_ReturnsError(t *testing.T) {
	yaml := `version: 1
environment:
  name: local
bogusKey: true
`
	p := writeConfig(t, yaml)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error for an unknown top-level key")
	}
	if !strings.Contains(err.Error(), "bogusKey") {
		t.Fatalf("error %q does not name the unknown key", err.Error())
	}
}

func TestLoad_UnknownTargetKey_ReturnsError(t *testing.T) {
	yaml := `version: 1
environment:
  name: local
targets:
  wiremock:
    url: http://127.0.0.1:9090
    adminUrl: http://127.0.0.1:9090/__admin
  fake-service:
    url: http://127.0.0.1:1234
`
	p := writeConfig(t, yaml)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error for an unknown target key")
	}
	if !strings.Contains(err.Error(), "fake-service") {
		t.Fatalf("error %q does not name the unknown key", err.Error())
	}
}

func TestLoad_MissingVersion_ReturnsError(t *testing.T) {
	yaml := `environment:
  name: local
`
	p := writeConfig(t, yaml)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error for a missing version")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("error %q does not mention version", err.Error())
	}
}
