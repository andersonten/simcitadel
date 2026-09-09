package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/db"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// Preflight checks the health of each target the scenarios reference before the run.
// Preflight returns an EnvironmentError if any referenced target is unreachable
// or unhealthy. The databases parameter holds the open connections that the
// SELECT 1 check of SDD section 4.10 uses, keyed by target name.
func Preflight(ctx context.Context, cfg *config.CitadelConfig, httpClient *http.Client, scenarios []*scenario.Scenario, databases map[string]*sql.DB) error {
	refs := referencedTargets(scenarios, cfg)

	if refs.wiremock && cfg.Targets.WireMock != nil && cfg.Targets.WireMock.AdminURL != "" {
		healthURL := cfg.Targets.WireMock.AdminBase() + "/health"
		if err := checkHealth(ctx, httpClient, healthURL, "wiremock"); err != nil {
			return err
		}
	}

	if refs.eventTrigger && cfg.Targets.EventTrigger != nil && cfg.Targets.EventTrigger.URL != "" {
		url := strings.TrimRight(cfg.Targets.EventTrigger.URL, "/") + "/health"
		if err := checkHealthBrokerConnected(ctx, httpClient, url, "event-trigger"); err != nil {
			return err
		}
	}

	if refs.eventCapture && cfg.Targets.EventCapture != nil && cfg.Targets.EventCapture.URL != "" {
		url := strings.TrimRight(cfg.Targets.EventCapture.URL, "/") + "/health"
		if err := checkSpyQueuesBound(ctx, httpClient, url, cfg.Capture.Exchanges); err != nil {
			return err
		}
	}

	if refs.rabbitmq && cfg.Targets.RabbitMQ != nil && cfg.Targets.RabbitMQ.ManagementURL != "" {
		mgmt := managementFor(cfg.Targets.RabbitMQ)
		if err := checkManagementHealth(ctx, httpClient, mgmt); err != nil {
			return err
		}
		if err := checkLockQueueDeclared(ctx, httpClient, mgmt); err != nil {
			return err
		}
	}

	if refs.app && cfg.Targets.App != nil && cfg.Targets.App.URL != "" {
		readinessPath := cfg.Targets.App.ReadinessPath
		if readinessPath == "" {
			readinessPath = "/health"
		}
		url := strings.TrimRight(cfg.Targets.App.URL, "/") + readinessPath
		timeout := cfg.Defaults.AppReadyTimeout.Duration()
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		if err := checkAppReady(ctx, httpClient, url, timeout); err != nil {
			return err
		}
	}

	if err := checkDatabasesAnswer(ctx, cfg, databases, refs.databaseNames()); err != nil {
		return err
	}

	if refs.debezium && cfg.Targets.DebeziumOutbox != nil && cfg.Targets.DebeziumOutbox.URL != "" {
		if err := checkDebeziumRunning(ctx, httpClient, cfg.Targets.DebeziumOutbox); err != nil {
			return err
		}
	}

	slog.Info("preflight checks passed", "targets", refs.summary())
	return nil
}

// checkSpyQueuesBound reads the capture health report and holds every
// configured exchange to the bound state. SDD section 8.1 treats anything but
// bound as not ready, because an unbound spy queue fails an outbound assertion
// for a reason that has nothing to do with the application.
func checkSpyQueuesBound(ctx context.Context, client *http.Client, url string, exchanges []config.CaptureExchange) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight event-capture: build request: %v", err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight event-capture: unreachable: %v", err)}
	}
	defer resp.Body.Close()

	// A 503 is the service reporting itself degraded, and its body names the
	// exchange that is wrong. Returning on the status code alone would trade the
	// message SDD section 4.10 asks for against a number.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight event-capture: status %d, expect 200 or 503", resp.StatusCode)}
	}

	// The shape follows the health handler of internal/capture, which reports
	// one state per exchange under the exchanges key.
	var body struct {
		Status    string            `json:"status"`
		Exchanges map[string]string `json:"exchanges"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight event-capture: decode health response: %v", err)}
	}

	for _, exchange := range exchanges {
		state := body.Exchanges[exchange.Name]
		if state == "bound" {
			continue
		}
		if state == "" {
			state = "absent"
		}
		return &EnvironmentError{Msg: fmt.Sprintf(
			"preflight event-capture: spy queue for exchange %s reports %s, expect bound; the app can still be starting",
			exchange.Name, state)}
	}
	return nil
}

// checkLockQueueDeclared verifies through the management API that the lock
// queue of SDD section 4.8 exists. Phase 3 declares it, so a run that reaches
// phase 4 without it holds no lock.
func checkLockQueueDeclared(ctx context.Context, client *http.Client, mgmt management) error {
	name := citadelamqp.LockQueueName()

	req, err := mgmt.request(ctx, http.MethodGet,
		fmt.Sprintf("/api/queues/%%2F/%s", url.PathEscape(name)), nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight rabbitmq: build lock queue request: %v", err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight rabbitmq: read lock queue %s: %v", name, err)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight rabbitmq: lock queue %s is not declared", name)}
	}
	if resp.StatusCode != http.StatusOK {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight rabbitmq: lock queue %s returned status %d", name, resp.StatusCode)}
	}
	return nil
}

// checkDatabasesAnswer runs SELECT 1 on every database target that the run
// references and the configuration declares. A target the configuration does
// not declare is a validation matter, not an environment one.
func checkDatabasesAnswer(ctx context.Context, cfg *config.CitadelConfig, databases map[string]*sql.DB, names []string) error {
	configured := databaseTargets(cfg)

	for _, name := range names {
		if _, declared := configured[name]; !declared {
			continue
		}
		handle := databases[name]
		if handle == nil {
			return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: the target is configured but not connected", name)}
		}
		var one int
		if err := handle.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: SELECT 1 failed: %v", name, err)}
		}
		slog.Info("preflight database answered", "target", name)
	}
	return nil
}

// checkDebeziumRunning holds the last row of SDD section 4.10. The health
// endpoint must report UP and the connector must report RUNNING, because
// Debezium reports healthy while a connector has failed.
func checkDebeziumRunning(ctx context.Context, client *http.Client, target *config.DebeziumOutboxTarget) error {
	baseURL := strings.TrimRight(target.URL, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/q/health", nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: build request: %v", err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: unreachable: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: health status %d, expect 200", resp.StatusCode)}
	}

	var health struct {
		Status string `json:"status"`
		Checks []struct {
			Name   string         `json:"name"`
			Status string         `json:"status"`
			Data   map[string]any `json:"data"`
		} `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: decode health response: %v", err)}
	}
	if health.Status != "UP" {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: health reports %q, expect UP", health.Status)}
	}

	state := ""
	for _, check := range health.Checks {
		if found := connectorState(check.Data, target.ConnectorName); found != "" {
			state = found
			break
		}
	}
	if state == "" {
		found, err := connectorStatusEndpoint(ctx, client, baseURL, target.ConnectorName)
		if err != nil {
			return err
		}
		state = found
	}

	if state == "" {
		return &EnvironmentError{Msg: fmt.Sprintf(
			"preflight debezium: health is UP but no connector state is reported at %s; health alone is not evidence that the connector runs",
			baseURL)}
	}
	if state != "RUNNING" {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: connector state is %q, expect RUNNING", state)}
	}
	return nil
}

// connectorState reads a connector state out of one health check payload.
// The payload names the state under a state key, or under the connector name.
func connectorState(data map[string]any, connectorName string) string {
	if data == nil {
		return ""
	}
	keys := []string{"state", "connector.state"}
	if connectorName != "" {
		keys = append(keys, connectorName)
	}
	for _, key := range keys {
		if value, ok := data[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// connectorStatusEndpoint reads the connector state from the Kafka Connect
// status endpoint, which a Debezium deployment behind Connect exposes.
// It returns an empty state when the deployment has no such endpoint.
func connectorStatusEndpoint(ctx context.Context, client *http.Client, baseURL, connectorName string) (string, error) {
	if connectorName == "" {
		return "", nil
	}

	statusURL := fmt.Sprintf("%s/connectors/%s/status", baseURL, url.PathEscape(connectorName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return "", &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: build connector status request: %v", err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: read connector %s status: %v", connectorName, err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil
	}

	var status struct {
		Connector struct {
			State string `json:"state"`
		} `json:"connector"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", &EnvironmentError{Msg: fmt.Sprintf("preflight debezium: decode connector status: %v", err)}
	}
	return status.Connector.State, nil
}

func checkHealth(ctx context.Context, client *http.Client, url, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: build request: %v", target, err)}
	}
	return runHealthRequest(client, req, target)
}

// checkManagementHealth is the same check against the RabbitMQ management API,
// which needs the credentials of SDD section 5.1.
func checkManagementHealth(ctx context.Context, client *http.Client, mgmt management) error {
	const target = "rabbitmq-management"

	req, err := mgmt.request(ctx, http.MethodGet, "/api/overview", nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: build request: %v", target, err)}
	}
	return runHealthRequest(client, req, target)
}

// runHealthRequest holds the shared body of the two checks above.
func runHealthRequest(client *http.Client, req *http.Request, target string) error {
	resp, err := client.Do(req)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: unreachable: %v", target, err)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: status %d, expect 200", target, resp.StatusCode)}
	}
	return nil
}

func checkHealthBrokerConnected(ctx context.Context, client *http.Client, url, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: build request: %v", target, err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: unreachable: %v", target, err)}
	}
	defer resp.Body.Close()

	// A 503 is the service reporting itself degraded, and its body names the
	// broker state. See checkSpyQueuesBound above for the same reasoning.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: status %d, expect 200 or 503", target, resp.StatusCode)}
	}

	var body struct {
		Broker string `json:"broker"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: decode health response: %v", target, err)}
	}
	if body.Broker != "connected" {
		return &EnvironmentError{Msg: fmt.Sprintf("preflight %s: broker status is %q, expect connected", target, body.Broker)}
	}
	return nil
}

func checkAppReady(ctx context.Context, client *http.Client, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return &EnvironmentError{Msg: fmt.Sprintf("preflight app: build request: %v", err)}
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return &EnvironmentError{Msg: fmt.Sprintf("preflight app: not ready after %s", timeout)}
		}

		select {
		case <-ctx.Done():
			return &EnvironmentError{Msg: fmt.Sprintf("preflight app: context cancelled: %v", ctx.Err())}
		case <-time.After(interval):
		}
	}
}

// Reset performs the pre-run reset sequence.
// Reset follows the fixed order from SDD section 4.9.
// The databases parameter holds open connections keyed by target name.
// The truncateTables parameter holds tables to truncate after the budget reset.
func Reset(ctx context.Context, cfg *config.CitadelConfig, runID string, httpClient *http.Client, databases map[string]*sql.DB, truncateTables ...string) error {
	// Step 3 purges every queue through the management API and step 7 verifies
	// the depths there. A broker with no management URL therefore skips both,
	// and the sequence of SDD section 4.9 is fixed and unconditional. A run that
	// starts against queues it did not purge sees a message it never sent.
	// Step 3 purges every queue through the management API and step 7 verifies
	// the depths there. A broker with no management URL therefore skips both,
	// and the sequence of SDD section 4.9 is fixed and unconditional. A run that
	// starts against queues it did not purge sees a message it never sent.

	if cfg.Targets.RabbitMQ != nil && cfg.Targets.RabbitMQ.ManagementURL == "" {
		return &InvalidInputError{Msg: "targets.rabbitmq.managementUrl is required, " +
			"because the reset purges every queue through it and verifies the depths afterwards"}
	}

	if cfg.Targets.WireMock != nil && cfg.Targets.WireMock.AdminURL != "" {
		if err := resetWireMock(ctx, cfg.Targets.WireMock.AdminBase(), httpClient); err != nil {
			return err
		}
	}

	var purged []string
	if cfg.Targets.RabbitMQ != nil && cfg.Targets.RabbitMQ.ManagementURL != "" {
		names, err := purgeQueues(ctx, cfg, httpClient)
		if err != nil {
			return err
		}
		purged = names
	}

	if cfg.Targets.EventCapture != nil && cfg.Targets.EventCapture.URL != "" {
		if err := resetCapture(ctx, cfg.Targets.EventCapture.URL, httpClient); err != nil {
			return err
		}
	}

	if cfg.Targets.EventTrigger != nil && cfg.Targets.EventTrigger.URL != "" {
		if err := resetBudget(ctx, cfg.Targets.EventTrigger.URL, runID, httpClient); err != nil {
			return err
		}
	}

	if err := ResetTruncate(ctx, databases, truncateTables); err != nil {
		return err
	}

	settle := cfg.Defaults.ResetSettle.Duration()
	if settle > 0 {
		slog.Info("reset settle wait", "duration", settle.String())
		select {
		case <-ctx.Done():
			return &EnvironmentError{Msg: fmt.Sprintf("reset settle interrupted: %v", ctx.Err())}
		case <-time.After(settle):
		}
	}

	if cfg.Targets.RabbitMQ != nil && cfg.Targets.RabbitMQ.ManagementURL != "" {
		if err := verifyQueueDepths(ctx, cfg.Targets.RabbitMQ, httpClient, purged); err != nil {
			return err
		}
	}

	checkReplicationSlots(ctx, cfg, databases)

	return nil
}

// ResetTruncate truncates the tables that the scenario reset spec names.
// A scenario that asks for truncation without a reachable app database is an
// environment failure, because a silent skip produces a run against stale rows.
func ResetTruncate(ctx context.Context, databases map[string]*sql.DB, tables []string) error {
	if len(tables) == 0 {
		return nil
	}

	handle, ok := databases["app-db"]
	if !ok || handle == nil {
		return &EnvironmentError{
			Msg: fmt.Sprintf("reset truncates %d table(s) but the app-db target is not connected", len(tables)),
		}
	}

	for _, table := range tables {
		if err := validateTableName(table); err != nil {
			return &InvalidInputError{Msg: fmt.Sprintf("reset truncate: %v", err)}
		}
		query := fmt.Sprintf("TRUNCATE TABLE %s", quoteTableName(table))
		if _, err := handle.ExecContext(ctx, query); err != nil {
			return &EnvironmentError{Msg: fmt.Sprintf("truncate table %s: %v", table, err)}
		}
		slog.Info("table truncated", "table", table)
	}
	return nil
}

// tableNamePattern accepts an optionally schema-qualified identifier.
var tableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)?$`)

// validateTableName rejects a table name that is not a plain identifier.
// A TRUNCATE target cannot be a bound parameter, so the name is checked
// instead of escaped.
func validateTableName(table string) error {
	if !tableNamePattern.MatchString(table) {
		return fmt.Errorf("table name %q is not a plain identifier", table)
	}
	return nil
}

// quoteTableName double-quotes each part of a possibly schema-qualified name.
func quoteTableName(table string) string {
	parts := strings.Split(table, ".")
	for i, part := range parts {
		parts[i] = `"` + part + `"`
	}
	return strings.Join(parts, ".")
}

// verifyQueueDepths checks that every queue the reset purged reports depth 0.
// A queue the reset did not purge is out of scope, because a producer that is
// not part of this run may legitimately have written to it.
func verifyQueueDepths(ctx context.Context, target *config.RabbitMQTarget, client *http.Client, purged []string) error {
	if len(purged) == 0 {
		return nil
	}

	entries, err := queueDepths(ctx, target, client)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("check queue depths: %v", err)}
	}

	inScope := make(map[string]bool, len(purged))
	for _, name := range purged {
		inScope[name] = true
	}

	for _, entry := range entries {
		if !inScope[entry.Name] {
			continue
		}
		if entry.Messages > 0 {
			return &EnvironmentError{Msg: fmt.Sprintf("queue %s reports depth %d after purge, expect 0", entry.Name, entry.Messages)}
		}
	}

	slog.Info("every purged queue reports depth 0 after settle", "queues", len(purged))
	return nil
}

func checkReplicationSlots(ctx context.Context, cfg *config.CitadelConfig, databases map[string]*sql.DB) {
	if cfg.Targets.DebeziumOutbox == nil || cfg.Targets.DebeziumOutbox.URL == "" {
		return
	}

	handle, ok := databases["app-db"]
	if !ok || handle == nil {
		return
	}

	// A replication slot is a Postgres object. SDD section 10.2 allows a SQL
	// Server application database, and firing this query at one warns on every
	// run about a table that dialect never has.
	if target := cfg.Targets.AppDB; target != nil {
		dialect, err := db.DialectOf(target.Driver)
		if err != nil || dialect == db.DialectSQLServer {
			return
		}
	}

	rows, err := handle.QueryContext(ctx, "SELECT slot_name FROM pg_replication_slots")
	if err != nil {
		slog.Warn("cannot query pg_replication_slots", "error", err.Error())
		return
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		slog.Warn("pre-existing replication slot detected; run 'docker compose down -v' to remove", "slot", name)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("iterate replication slots failed", "error", err.Error())
	}
}

func resetWireMock(ctx context.Context, adminURL string, client *http.Client) error {
	scenariosURL := strings.TrimRight(adminURL, "/") + "/scenarios/reset"
	if err := postEmpty(ctx, client, scenariosURL); err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("reset wiremock scenarios: %v", err)}
	}

	// The journal is cleared with DELETE /__admin/requests. The pinned WireMock
	// 3.12.1 does not serve POST /__admin/requests/reset and answers 404, so the
	// earlier path failed every reset and made citadel run exit 3 on every run
	// against the stack this repository ships.
	requestsURL := strings.TrimRight(adminURL, "/") + "/requests"
	if err := deleteEmpty(ctx, client, requestsURL); err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("reset wiremock requests: %v", err)}
	}

	slog.Info("wiremock scenarios and requests reset")
	return nil
}

// purgeQueues empties every queue in the environment and returns the names it
// purged, so the settle check of SDD section 4.9 step 7 can verify exactly
// those queues and no others.
func purgeQueues(ctx context.Context, cfg *config.CitadelConfig, client *http.Client) ([]string, error) {
	mgmt := managementFor(cfg.Targets.RabbitMQ)

	queues, err := listQueues(ctx, cfg.Targets.RabbitMQ, client)
	if err != nil {
		return nil, &EnvironmentError{Msg: fmt.Sprintf("list rabbitmq queues: %v", err)}
	}

	purged := make([]string, 0, len(queues))
	for _, q := range queues {
		// The lock queue is not test data. Step 3 of SDD section 4.9 names the
		// queues it means, and the lock is none of them: it is the mechanism that
		// stops two runs sharing an environment. Section 4.8 declares it exclusive,
		// so the management API answers 400 while a run holds it, and reset runs
		// during a run by definition.
		if q == citadelamqp.LockQueueName() {
			continue
		}
		req, err := mgmt.request(ctx, http.MethodDelete,
			fmt.Sprintf("/api/queues/%%2F/%s/contents", url.PathEscape(q)), nil)
		if err != nil {
			return nil, &EnvironmentError{Msg: fmt.Sprintf("build purge request for queue %s: %v", q, err)}
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, &EnvironmentError{Msg: fmt.Sprintf("purge queue %s: %v", q, err)}
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return nil, &EnvironmentError{Msg: fmt.Sprintf("purge queue %s: status %d", q, resp.StatusCode)}
		}
		purged = append(purged, q)
		slog.Info("queue purged", "queue", q)
	}
	return purged, nil
}

// queueDepth pairs a queue name with the number of messages it holds.
type queueDepth struct {
	Name     string `json:"name"`
	Messages int    `json:"messages"`
}

// queueDepths reads every queue and its depth from the management API.
func queueDepths(ctx context.Context, target *config.RabbitMQTarget, client *http.Client) ([]queueDepth, error) {
	req, err := managementFor(target).request(ctx, http.MethodGet, "/api/queues", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list queues returned status %d", resp.StatusCode)
	}

	var entries []queueDepth
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode queue list: %w", err)
	}
	return entries, nil
}

func listQueues(ctx context.Context, target *config.RabbitMQTarget, client *http.Client) ([]string, error) {
	entries, err := queueDepths(ctx, target, client)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names, nil
}

func resetCapture(ctx context.Context, captureURL string, client *http.Client) error {
	url := strings.TrimRight(captureURL, "/") + "/captured"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("build capture reset request: %v", err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("reset captured messages: %v", err)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return &EnvironmentError{Msg: fmt.Sprintf("reset captured messages: status %d", resp.StatusCode)}
	}
	slog.Info("captured messages cleared")
	return nil
}

func resetBudget(ctx context.Context, triggerURL, runID string, client *http.Client) error {
	url := strings.TrimRight(triggerURL, "/") + "/budget/reset"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("build budget reset request: %v", err)}
	}
	req.Header.Set("X-Citadel-Run", runID)
	resp, err := client.Do(req)
	if err != nil {
		return &EnvironmentError{Msg: fmt.Sprintf("reset trigger budget: %v", err)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return &EnvironmentError{Msg: fmt.Sprintf("reset trigger budget: status %d", resp.StatusCode)}
	}
	slog.Info("trigger budget reset", "runId", runID)
	return nil
}

func postEmpty(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("POST %s returned status %d", url, resp.StatusCode)
	}
	return nil
}

// deleteEmpty sends a DELETE with no body. The WireMock journal is a collection,
// and this version deletes it rather than accept a reset action on it.
func deleteEmpty(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE %s returned status %d", url, resp.StatusCode)
	}
	return nil
}
