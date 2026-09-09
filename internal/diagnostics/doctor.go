package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/db"
)

const checkTimeout = 5 * time.Second

// The three statuses that SDD section 12.3 allows for a check.
const (
	statusPass = "pass"
	statusFail = "fail"
	statusSkip = "skip"
)

// requiredDebeziumImageTag is the image tag that SDD section 10.1 pins. The
// app devops template pins the same tag. A newer Debezium means the local
// outbound pipeline is not the pipeline that production runs.
const requiredDebeziumImageTag = "3.2.0.Final"

// The Debezium environment variable names that SDD section 10.1 requires.
const (
	heartbeatIntervalKey   = "DEBEZIUM_SOURCE_HEARTBEAT_INTERVAL_MS"
	routingKeySourceKey    = "DEBEZIUM_SINK_RABBITMQ_ROUTINGKEY_SOURCE"
	routingKeyFromTopicKey = "DEBEZIUM_SINK_RABBITMQ_ROUTINGKEYFROMTOPICNAME"
)

// The SQL that the database checks run against the app database.
const (
	walLevelQuery             = `SHOW wal_level`
	replicationSlotQuery      = `SELECT slot_name, active FROM pg_replication_slots`
	inboxColumnQuery          = `SELECT table_schema, column_name FROM information_schema.columns WHERE table_name = $1`
	inboxColumnQuerySQLServer = `SELECT table_schema, column_name FROM information_schema.columns WHERE table_name = @p1`
)

// selectOneQuery is the reachability probe of the preflight table in SDD
// section 4.10. Both supported engines answer it.
const selectOneQuery = `SELECT 1`

// The SQL that the two SQL Server checks run. sys.dm_server_services reports
// the state of the Agent service, and msdb.dbo.cdc_jobs holds the recorded
// poll interval of the capture job.
const (
	sqlAgentQuery   = `SELECT servicename, status_desc FROM sys.dm_server_services WHERE servicename LIKE 'SQL Server Agent%'`
	captureJobQuery = `SELECT pollinginterval FROM msdb.dbo.cdc_jobs WHERE job_type = 'capture'`
)

// defaultCaptureJobPollInterval is the poll interval in seconds that SQL Server
// gives a new capture job. SDD section 10.2 needs a reduced value, so this
// value proves that nobody reduced it.
const defaultCaptureJobPollInterval = 5

// captureJobRestartRemediation carries the stop and the start that SDD section
// 10.2 calls out. sys.sp_cdc_change_job writes the row in msdb.dbo.cdc_jobs and
// the live job keeps its old interval until it stops and starts again.
const captureJobRestartRemediation = "Run 'EXEC sys.sp_cdc_change_job @job_type = N''capture'', @pollinginterval = 1', then 'EXEC sys.sp_cdc_stop_job @job_type = N''capture''' and 'EXEC sys.sp_cdc_start_job @job_type = N''capture'''. The job records the new interval and ignores it until the stop and the start run."

// sqlAgentRemediation names the compose setting that starts the Agent.
const sqlAgentRemediation = "Set MSSQL_AGENT_ENABLED to true for the cdc-sqlserver compose service and recreate the container. The capture job is an Agent job, so CDC emits no change while the Agent is down."

// walLevelRemediation carries the command override of SDD section 10.2.
const walLevelRemediation = `Set the Postgres command override to command: ["postgres", "-c", "wal_level=logical"] in the compose file, then recreate the container.`

// managementAccessRemediation covers a management API that answers nothing
// useful. The plugin rejects an anonymous request, so a credential is as likely
// a cause as a broker that is down.
const managementAccessRemediation = "Start RabbitMQ, then verify targets.rabbitmq.managementUrl and the credentials in targets.rabbitmq.amqpUrl. The management plugin rejects an anonymous request with status 401."

// staleSlotRemediation carries the true reset of SDD section 4.9.
const staleSlotRemediation = "Run 'docker compose down -v'. A restart alone keeps the volume and the slot, so it is not a true reset."

// CheckResult holds the outcome of a single doctor check.
type CheckResult struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

// RunDoctor performs all doctor checks against the given configuration.
// RunDoctor returns one result for each row of SDD section 12.3.
func RunDoctor(ctx context.Context, cfg *config.CitadelConfig) []CheckResult {
	var results []CheckResult
	results = append(results, checkConfig(cfg))
	results = append(results, checkTargets(ctx, cfg)...)
	results = append(results, checkAppReadiness(ctx, cfg))
	results = append(results, checkAlternateExchange(ctx, cfg))
	results = append(results, checkHydraToken(ctx, cfg))
	results = append(results, checkSpyQueues(ctx, cfg))
	results = append(results, checkDebeziumConnector(ctx, cfg))
	results = append(results, checkDebeziumImageTag(ctx, cfg))
	results = append(results, checkHeartbeatDisabled(ctx, cfg))
	results = append(results, checkRoutingKeySource(ctx, cfg))
	results = append(results, checkAutoCreateRoutingKeyQueues(ctx, cfg))
	results = append(results, checkInboxTable(ctx, cfg))
	results = append(results, checkWalLevel(ctx, cfg))
	results = append(results, checkStaleReplicationSlot(ctx, cfg))
	results = append(results, checkSQLAgent(ctx, cfg))
	results = append(results, checkCaptureJobPollInterval(ctx, cfg))
	return results
}

// ConfigLoadFailure returns the first row of the SDD section 12.3 table for a
// configuration that does not load. config.Load names the unset variable in its
// error, and this carries that error into the check stream with the remediation
// that section 12.3 requires of every failure. A caller that reports the load
// error alone leaves the row with no result at all, and SDD section 5.2 makes
// doctor the command that "tells them exactly which are missing".
func ConfigLoadFailure(err error) CheckResult {
	return CheckResult{
		Name:        "config-valid",
		Status:      statusFail,
		Message:     fmt.Sprintf("The configuration does not load: %s", RedactString(err.Error())),
		Remediation: "Export the named variable in the shell that runs citadel, or correct the configuration file. Section 5.2 reads no .env file, so the shell is the only source, and a referenced variable that is unset is fatal rather than an empty string.",
	}
}

// checkConfig confirms that the configuration reached doctor. config.Load
// names an unset variable and returns an error, so an unresolved reference
// reaches ConfigLoadFailure instead of this check.
func checkConfig(cfg *config.CitadelConfig) CheckResult {
	if cfg == nil {
		return CheckResult{
			Name:        "config-valid",
			Status:      statusFail,
			Message:     "Configuration is nil, so no variable resolved.",
			Remediation: "Load the configuration file with config.Load before you run doctor checks.",
		}
	}
	return CheckResult{
		Name:    "config-valid",
		Status:  statusPass,
		Message: "Configuration parsed and every ${VAR} reference resolved.",
	}
}

// httpTarget pairs a target name with the address that doctor tries.
type httpTarget struct {
	name string
	url  string
}

// namedDatabase pairs a database target with the name that a scenario step and
// the preflight of SDD section 4.10 use for it.
type namedDatabase struct {
	name   string
	target *config.DatabaseTarget
}

// httpTargets returns every configured HTTP target in a fixed order.
//
// Each address is the health endpoint that the preflight table of SDD section
// 4.10 names, and not the root of the service. SDD section 4.1 states that
// doctor checks every target "reachable and correctly set up", and the root of
// the trigger and of the capture service answers 404 from its own router while
// the service reports itself degraded at /health. A probe of the root therefore
// reports an all-clear for a trigger whose broker connection is down, and the
// run that follows fails preflight at exit 3.
func httpTargets(cfg *config.CitadelConfig) []httpTarget {
	var targets []httpTarget
	if cfg.Targets.WireMock != nil && cfg.Targets.WireMock.AdminURL != "" {
		targets = append(targets, httpTarget{name: "wiremock", url: cfg.Targets.WireMock.AdminBase() + "/health"})
	}
	if cfg.Targets.EventTrigger != nil && cfg.Targets.EventTrigger.URL != "" {
		targets = append(targets, httpTarget{name: "event-trigger", url: healthAddress(cfg.Targets.EventTrigger.URL)})
	}
	if cfg.Targets.EventCapture != nil && cfg.Targets.EventCapture.URL != "" {
		targets = append(targets, httpTarget{name: "event-capture", url: healthAddress(cfg.Targets.EventCapture.URL)})
	}
	if cfg.Targets.RabbitMQ != nil && cfg.Targets.RabbitMQ.ManagementURL != "" {
		targets = append(targets, httpTarget{name: "rabbitmq-management", url: cfg.Targets.RabbitMQ.ManagementURL})
	}
	if cfg.Targets.DebeziumOutbox != nil && cfg.Targets.DebeziumOutbox.URL != "" {
		targets = append(targets, httpTarget{name: "debezium-outbox", url: cfg.Targets.DebeziumOutbox.URL})
	}
	return targets
}

// healthAddress returns the /health endpoint of a service address.
func healthAddress(addr string) string {
	return strings.TrimRight(addr, "/") + "/health"
}

// databaseTargets returns every configured database target in a fixed order.
// The names match internal/runner, so doctor and the preflight of SDD section
// 4.10 name the same target.
func databaseTargets(cfg *config.CitadelConfig) []namedDatabase {
	var targets []namedDatabase
	if cfg.Targets.AppDB != nil && cfg.Targets.AppDB.DSN != "" {
		targets = append(targets, namedDatabase{name: "app-db", target: cfg.Targets.AppDB})
	}
	if cfg.Targets.UpstreamShadow != nil && cfg.Targets.UpstreamShadow.DSN != "" {
		targets = append(targets, namedDatabase{name: "upstream-shadow", target: cfg.Targets.UpstreamShadow})
	}
	return targets
}

// checkTargets covers the reachability row of SDD section 12.3. The preflight
// table of section 4.10 names app-db and upstream-shadow next to the HTTP
// targets, so a database target needs its own probe. Nothing else in doctor
// opens the shadow.
func checkTargets(ctx context.Context, cfg *config.CitadelConfig) []CheckResult {
	if cfg == nil {
		return nil
	}

	addresses := httpTargets(cfg)
	databases := databaseTargets(cfg)

	if len(addresses) == 0 && len(databases) == 0 {
		return []CheckResult{{
			Name:    "targets-reachable",
			Status:  statusSkip,
			Message: "The configuration names no target, so doctor has no address to try.",
		}}
	}

	var results []CheckResult
	for _, t := range addresses {
		results = append(results, probeTarget(ctx, t.name, t.url))
	}
	for _, d := range databases {
		results = append(results, probeDatabase(ctx, d.name, d.target))
	}
	return results
}

// probeDatabase proves a database target answers SELECT 1, which is the
// preflight of SDD section 4.10. The message names the target and the address
// that doctor tried. RedactString removes the password from that address,
// because a doctor report reaches a chat window as often as a bundle does.
func probeDatabase(ctx context.Context, name string, target *config.DatabaseTarget) CheckResult {
	checkName := fmt.Sprintf("target-%s-reachable", name)
	address := RedactString(target.DSN)

	handle, err := openDatabase(ctx, target)
	if err != nil {
		return CheckResult{
			Name:        checkName,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot reach the database target %s at %s: %s", name, address, RedactString(err.Error())),
			Remediation: fmt.Sprintf("Start the %s database, then verify targets.%s.driver and targets.%s.dsn in the configuration.", name, name, name),
		}
	}
	defer handle.Close()

	queryCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	var one int
	if err := handle.QueryRowContext(queryCtx, selectOneQuery).Scan(&one); err != nil {
		return CheckResult{
			Name:        checkName,
			Status:      statusFail,
			Message:     fmt.Sprintf("The database target %s at %s accepted a connection and SELECT 1 failed: %s", name, address, RedactString(err.Error())),
			Remediation: fmt.Sprintf("Grant the role in targets.%s.dsn permission to run a query, then run doctor again.", name),
		}
	}

	return CheckResult{
		Name:    checkName,
		Status:  statusPass,
		Message: fmt.Sprintf("The database target %s at %s answered SELECT 1.", name, address),
	}
}

func probeTarget(ctx context.Context, name, addr string) CheckResult {
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, addr, nil)
	if err != nil {
		return CheckResult{
			Name:        fmt.Sprintf("target-%s-reachable", name),
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot build request for %s at %s: %s", name, addr, err),
			Remediation: fmt.Sprintf("Verify the URL %s in the configuration.", addr),
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{
			Name:        fmt.Sprintf("target-%s-reachable", name),
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot reach %s at %s: %s", name, addr, err),
			Remediation: fmt.Sprintf("Start the %s service or verify the URL %s.", name, addr),
		}
	}
	resp.Body.Close()

	// SDD section 12.3 does not define reachable, so doctor reads it as the
	// narrower state: a 5xx answer means the service itself is broken and no
	// scenario can run against it. The app readiness check of the same table
	// reads the status code too, and this keeps the two siblings consistent.
	// A 4xx answer stays a pass, because the bare root of an admin API
	// answers 401 or 404 while the service is healthy. Only a readiness path
	// carries a 2xx contract, so only that check holds a target to 2xx.
	if resp.StatusCode >= 500 {
		return CheckResult{
			Name:        fmt.Sprintf("target-%s-reachable", name),
			Status:      statusFail,
			Message:     fmt.Sprintf("Target %s at %s responded with status %d, so the service answers and its own work fails.", name, addr, resp.StatusCode),
			Remediation: fmt.Sprintf("Read the log of the %s service for the error behind status %d, correct it, then run doctor again.", name, resp.StatusCode),
		}
	}

	return CheckResult{
		Name:    fmt.Sprintf("target-%s-reachable", name),
		Status:  statusPass,
		Message: fmt.Sprintf("Target %s at %s responded with status %d.", name, addr, resp.StatusCode),
	}
}

// checkAppReadiness covers the app readiness row of SDD section 12.3. The
// message names the endpoint that doctor tried.
func checkAppReadiness(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "app-readiness"
	if cfg == nil || cfg.Targets.App == nil || cfg.Targets.App.URL == "" {
		return CheckResult{
			Name:    name,
			Status:  statusSkip,
			Message: "App target is not configured.",
		}
	}

	readinessPath := cfg.Targets.App.ReadinessPath
	if readinessPath == "" {
		readinessPath = "/health"
	}

	endpoint := cfg.Targets.App.URL + readinessPath

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot build request for readiness endpoint %s: %s", endpoint, err),
			Remediation: "Verify the app URL and the readiness path in the configuration.",
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Readiness endpoint %s is not reachable: %s", endpoint, err),
			Remediation: "Start the application or verify the URL and the readiness path.",
		}
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Readiness endpoint %s returned status %d.", endpoint, resp.StatusCode),
			Remediation: fmt.Sprintf("Verify the application is healthy and that %s is the correct readiness path.", readinessPath),
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: fmt.Sprintf("Readiness endpoint %s returned status %d.", endpoint, resp.StatusCode),
	}
}

func checkAlternateExchange(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "alternate-exchange-policy"
	if cfg == nil || cfg.Targets.RabbitMQ == nil || cfg.Targets.RabbitMQ.ManagementURL == "" {
		return CheckResult{
			Name:    name,
			Status:  statusSkip,
			Message: "RabbitMQ management URL is not configured.",
		}
	}

	policiesURL := RedactString(managementAddress(cfg.Targets.RabbitMQ, "/api/policies"))

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := managementRequest(checkCtx, cfg.Targets.RabbitMQ, "/api/policies")
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot build request for policies at %s: %s", policiesURL, err),
			Remediation: "Verify the RabbitMQ management URL in the configuration.",
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot reach RabbitMQ management API at %s: %s", policiesURL, err),
			Remediation: "Start RabbitMQ or verify the management URL.",
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("RabbitMQ management API at %s returned status %d.", policiesURL, resp.StatusCode),
			Remediation: "Verify the credentials in targets.rabbitmq.amqpUrl reach the management API. The management plugin rejects an anonymous request with status 401, and the management user needs the monitoring tag.",
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read policies response: %s", err),
			Remediation: "Verify the RabbitMQ management URL in the configuration.",
		}
	}

	var policies []map[string]any
	if err := json.Unmarshal(body, &policies); err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot parse policies response as JSON: %s", err),
			Remediation: "Verify the RabbitMQ management API returns valid JSON.",
		}
	}

	for _, p := range policies {
		policyName, _ := p["name"].(string)
		if policyName == "citadel-alternate-exchange" {
			return CheckResult{
				Name:    name,
				Status:  statusPass,
				Message: "Policy citadel-alternate-exchange exists.",
			}
		}
	}

	return CheckResult{
		Name:        name,
		Status:      statusFail,
		Message:     "Policy citadel-alternate-exchange not found, so an unroutable message disappears silently.",
		Remediation: `Apply the policy with PUT /api/policies/%2F/citadel-alternate-exchange and the body {"pattern":"^(public|private)\\.","apply-to":"exchanges","priority":0,"definition":{"alternate-exchange":"citadel.unroutable.ae"}}.`,
	}
}

// tokenResponse models the OAuth2 client-credentials response of Hydra.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
}

// checkHydraToken performs a real token acquisition, as SDD section 9 requires.
// A token with the wrong scopes fails later and less legibly than one that
// fails to issue, so the check reports the granted scopes.
func checkHydraToken(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "hydra-token"
	if cfg == nil || cfg.Targets.App == nil || cfg.Targets.App.Auth == "" {
		return CheckResult{Name: name, Status: statusSkip, Message: "No auth block configured for the app target."}
	}

	authName := cfg.Targets.App.Auth
	authCfg, ok := cfg.Auth[authName]
	if !ok {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Auth block %q referenced by the app target does not exist.", authName),
			Remediation: fmt.Sprintf("Add the auth block %q to the configuration.", authName),
		}
	}
	if authCfg.TokenURL == "" {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Auth block %q has no tokenUrl, so doctor cannot request a token.", authName),
			Remediation: fmt.Sprintf("Set tokenUrl in the auth block %q.", authName),
		}
	}

	var absent []string
	if strings.TrimSpace(authCfg.ClientID) == "" {
		absent = append(absent, "clientId")
	}
	if strings.TrimSpace(authCfg.ClientSecret) == "" {
		absent = append(absent, "clientSecret")
	}
	if len(absent) > 0 {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Hydra credentials are missing: auth block %q has no %s.", authName, strings.Join(absent, " and no ")),
			Remediation: fmt.Sprintf("Set %s in the auth block %q. Section 5.3 keeps the value in an environment variable, so verify that variable holds a value.", strings.Join(absent, " and "), authName),
		}
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", authCfg.ClientID)
	form.Set("client_secret", authCfg.ClientSecret)
	if len(authCfg.Scopes) > 0 {
		form.Set("scope", strings.Join(authCfg.Scopes, " "))
	}

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodPost, authCfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot build the token request for auth block %q: %s", authName, err),
			Remediation: fmt.Sprintf("Verify the tokenUrl %s in the auth block %q.", authCfg.TokenURL, authName),
		}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Hydra at %s is unreachable: %s. The consequence is no Hydra, no Citadel, because every run needs a token.", authCfg.TokenURL, err),
			Remediation: "Connect to the network that hosts the shared Hydra, or correct the tokenUrl. Citadel cannot run offline.",
		}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if readErr != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read the Hydra token response: %s", readErr),
			Remediation: fmt.Sprintf("Verify that %s is a token endpoint.", authCfg.TokenURL),
		}
	}

	if resp.StatusCode != http.StatusOK {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Hydra rejected the credentials of auth block %q with status %d. Hydra says: %s", authName, resp.StatusCode, strings.TrimSpace(string(body))),
			Remediation: fmt.Sprintf("Verify the clientId and the clientSecret of auth block %q, and verify the client exists in Hydra with the requested scopes.", authName),
		}
	}

	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot parse the Hydra token response as JSON: %s", err),
			Remediation: fmt.Sprintf("Verify that %s is a token endpoint.", authCfg.TokenURL),
		}
	}
	if token.AccessToken == "" {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Hydra accepted the credentials of auth block %q but the response holds an empty access_token.", authName),
			Remediation: fmt.Sprintf("Verify the grant type of the Hydra client for auth block %q. Citadel needs client_credentials.", authName),
		}
	}

	if token.Scope == "" {
		return CheckResult{
			Name:    name,
			Status:  statusPass,
			Message: fmt.Sprintf("Hydra issued a token for auth block %q. The response holds no scope field, so the granted scopes are unknown.", authName),
		}
	}

	withheld := withheldScopes(authCfg.Scopes, token.Scope)
	if len(withheld) > 0 {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Hydra issued a token for auth block %q with the granted scopes %q, and withheld the requested scopes %q.", authName, token.Scope, strings.Join(withheld, " ")),
			Remediation: fmt.Sprintf("Grant %s to the Hydra client of auth block %q. A token with the wrong scopes fails mid-scenario with a 403.", strings.Join(withheld, " and "), authName),
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: fmt.Sprintf("Hydra issued a token for auth block %q with the granted scopes %q.", authName, token.Scope),
	}
}

// withheldScopes returns each requested scope that the granted scope string
// does not hold.
func withheldScopes(requested []string, granted string) []string {
	if len(requested) == 0 {
		return nil
	}
	grantedSet := make(map[string]bool)
	for _, scope := range strings.Fields(granted) {
		grantedSet[scope] = true
	}
	var withheld []string
	for _, scope := range requested {
		scope = strings.TrimSpace(scope)
		if scope != "" && !grantedSet[scope] {
			withheld = append(withheld, scope)
		}
	}
	return withheld
}

// checkSpyQueues covers the spy queue row of SDD section 12.3. The check holds
// every exchange under capture.exchanges to the bound state, and it does not
// trust the exchange list that the capture service reports. A capture service
// that binds nothing reports an empty list, and that list alone proves nothing.
// Preflight in internal/runner compares against the same configured set, so
// citadel doctor and citadel run agree on the verdict.
func checkSpyQueues(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "spy-queues-bound"
	if cfg == nil || cfg.Targets.EventCapture == nil || cfg.Targets.EventCapture.URL == "" {
		return CheckResult{Name: name, Status: statusSkip, Message: "Event capture target is not configured."}
	}
	if len(cfg.Capture.Exchanges) == 0 {
		return CheckResult{Name: name, Status: statusSkip, Message: "The configuration names no exchange under capture.exchanges, so no spy queue exists to bind."}
	}
	healthURL := cfg.Targets.EventCapture.URL + "/health"
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot build request for %s: %s", healthURL, err), Remediation: "Verify the event-capture URL."}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot reach event-capture at %s: %s", healthURL, err), Remediation: "Start the event-capture service."}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot read the event-capture health response: %s", err), Remediation: "Verify the event-capture URL."}
	}
	// A 503 is the capture service reporting an exchange that is not bound, and
	// its body names which one. Returning here on the status code alone would
	// discard the detail this check exists to report.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Event-capture health at %s returned status %d.", healthURL, resp.StatusCode), Remediation: "Start the event-capture service or verify its URL."}
	}
	var health struct {
		Exchanges map[string]string `json:"exchanges"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot parse the event-capture health response as JSON: %s", err), Remediation: "Verify the event-capture URL."}
	}
	// A state that the report omits is absent and not bound. The capture
	// service reports one entry per exchange that it knows, so an exchange
	// with no entry is an exchange that the service never saw.
	var unbound []string
	for _, exchange := range cfg.Capture.Exchanges {
		state := health.Exchanges[exchange.Name]
		if state == "bound" {
			continue
		}
		if state == "" {
			state = "absent"
		}
		unbound = append(unbound, fmt.Sprintf("%s=%s", exchange.Name, state))
	}
	if len(unbound) > 0 {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("These exchanges of capture.exchanges are not bound: %s. The app can still be in start-up, because event-capture declares an exchange before it binds the spy queue.", strings.Join(unbound, ", ")),
			Remediation: "Wait for the app to finish start-up, then run doctor again. If the state holds, verify capture.exchanges names the exchange the app publishes to, and verify EVENT_CAPTURE_EXCHANGES of the compose service names the same exchange.",
		}
	}
	return CheckResult{Name: name, Status: statusPass, Message: fmt.Sprintf("All %d exchanges of capture.exchanges report a bound spy queue.", len(cfg.Capture.Exchanges))}
}

// quarkusHealth models the Microprofile health payload of Debezium Server.
// Debezium Server registers one liveness check named debezium. That check
// reports DOWN when the connector task stops or when the connector fails.
type quarkusHealth struct {
	Status string `json:"status"`
	Checks []struct {
		Name   string         `json:"name"`
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	} `json:"checks"`
}

// debeziumHealthCheckName is the name that Debezium Server gives its own
// liveness check. See io.debezium.server.ConnectorLifecycle.
const debeziumHealthCheckName = "debezium"

// checkDebeziumConnector reads the connector state and not only the top-level
// health status. SDD section 4.10 states that Debezium reports healthy while a
// connector has failed, so health alone is not evidence.
func checkDebeziumConnector(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "debezium-connector-running"
	if !debeziumConfigured(cfg) {
		return CheckResult{Name: name, Status: statusSkip, Message: "Debezium outbox target is not configured."}
	}

	connector := cfg.Targets.DebeziumOutbox.ConnectorName
	if connector == "" {
		connector = "outbox"
	}

	healthURL := strings.TrimRight(cfg.Targets.DebeziumOutbox.URL, "/") + "/q/health"
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot build request for %s: %s", healthURL, err), Remediation: "Verify the debezium-outbox URL in the configuration."}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot reach Debezium at %s: %s", healthURL, err), Remediation: "Start the Debezium service with the outbox compose profile."}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot read the Debezium health response: %s", err), Remediation: "Verify the debezium-outbox URL in the configuration."}
	}

	var health quarkusHealth
	if err := json.Unmarshal(body, &health); err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Debezium health at %s returned status %d and a body that is not health JSON: %s", healthURL, resp.StatusCode, strings.TrimSpace(string(body))),
			Remediation: "Verify the debezium-outbox URL points at the Debezium Server HTTP port.",
		}
	}

	// SDD section 4.10 joins the two conditions: the health endpoint reports UP
	// and the connector state is RUNNING. The preflight of internal/runner holds
	// a run to both, so a doctor that reads one of them passes where the run
	// fails at exit 3, and the developer who ran doctor first blames the
	// scenario. Quarkus answers 503 for a DOWN verdict, so the status code
	// carries the same fact and both reach the message.
	if resp.StatusCode != http.StatusOK || !strings.EqualFold(health.Status, "UP") {
		message := fmt.Sprintf("Debezium health at %s returned status %d and reports the overall verdict %s, and SDD section 4.10 needs UP. The connector state of %s is therefore not evidence.", healthURL, resp.StatusCode, health.Status, connector)
		if detail := debeziumCheckDetail(health); detail != "" {
			message += fmt.Sprintf(" Debezium reports: %s", detail)
		}
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     message,
			Remediation: fmt.Sprintf("Read the Debezium log for the check that reports DOWN, correct it, then verify %s answers status 200 with the overall verdict UP.", healthURL),
		}
	}

	for _, check := range health.Checks {
		if !strings.EqualFold(check.Name, debeziumHealthCheckName) {
			continue
		}

		// Debezium Server puts the connector state in the data of its own
		// health check. A state that the payload names wins over the UP of
		// the check itself, because the state is the fact that section 12.3
		// asks for and the check status is a summary of it.
		state := connectorStateOf(check.Data, connector)
		if state != "" && !strings.EqualFold(state, "RUNNING") {
			return CheckResult{
				Name:        name,
				Status:      statusFail,
				Message:     fmt.Sprintf("Debezium connector %s state is %s and SDD section 4.10 needs RUNNING. Overall health is %s and the health check %q reports %s. Debezium reports: %s", connector, state, health.Status, check.Name, check.Status, formatCheckData(check.Data)),
				Remediation: fmt.Sprintf("Read the Debezium connector log for the failure that stopped the task, then correct it and restart Debezium. Verify %s reports the state RUNNING afterwards.", healthURL),
			}
		}

		if strings.EqualFold(check.Status, "UP") {
			if state == "" {
				return CheckResult{
					Name:    name,
					Status:  statusPass,
					Message: fmt.Sprintf("Debezium connector %s state is RUNNING. The health check %q reports UP, and Debezium sets it from the connector task.", connector, check.Name),
				}
			}
			return CheckResult{
				Name:    name,
				Status:  statusPass,
				Message: fmt.Sprintf("Debezium connector %s reports the state %s, and the health check %q reports UP.", connector, state, check.Name),
			}
		}

		message := fmt.Sprintf("Debezium connector %s state is %s and SDD section 4.10 needs RUNNING. Overall health is %s.", connector, check.Status, health.Status)
		if data := formatCheckData(check.Data); data != "" {
			message += fmt.Sprintf(" Debezium reports: %s", data)
		} else {
			message += " Debezium reports no last error, so read the connector log for the cause."
		}
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     message,
			Remediation: fmt.Sprintf("Read the Debezium connector log for the failure that stopped the task, then correct it and restart Debezium. Verify %s answers UP for the check %q afterwards.", healthURL, debeziumHealthCheckName),
		}
	}

	return CheckResult{
		Name:        name,
		Status:      statusFail,
		Message:     fmt.Sprintf("Debezium health at %s reports %s but carries no check named %q, so the connector state is unknown. Readiness answers UP as soon as the process starts, so health alone is not evidence.", healthURL, health.Status, debeziumHealthCheckName),
		Remediation: fmt.Sprintf("Verify the debezium-outbox URL points at Debezium Server and not at another Quarkus process. Debezium Server publishes the check %q at /q/health.", debeziumHealthCheckName),
	}
}

// connectorStateOf reads the connector state out of one health check payload.
// Debezium names it under a state key or under the connector name. The
// preflight of internal/runner reads the same keys, and doctor keeps its own
// copy because internal/runner imports this package.
func connectorStateOf(data map[string]any, connectorName string) string {
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

// debeziumCheckDetail renders the data of the Debezium health check, so a
// failure of the overall verdict still quotes the connector detail.
func debeziumCheckDetail(health quarkusHealth) string {
	var parts []string
	for _, check := range health.Checks {
		detail := formatCheckData(check.Data)
		if detail == "" {
			parts = append(parts, fmt.Sprintf("%s=%s", check.Name, check.Status))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s (%s)", check.Name, check.Status, detail))
	}
	return strings.Join(parts, "; ")
}

// formatCheckData renders a health check data map in a stable order. Doctor
// quotes each value as Debezium reports it, per SDD section 12.3.
func formatCheckData(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, data[key]))
	}
	return strings.Join(parts, ", ")
}

// debeziumConfigured reports whether the configuration names a Debezium target.
func debeziumConfigured(cfg *config.CitadelConfig) bool {
	return cfg != nil && cfg.Targets.DebeziumOutbox != nil && cfg.Targets.DebeziumOutbox.URL != ""
}

// declaredDebezium holds the Debezium configuration that a compose file
// declares. Debezium Server serves only the Microprofile health endpoint, so
// no HTTP endpoint reports the image tag, the heartbeat interval or the
// routing key source. The declared file is the only source of truth for these
// three checks, and every message names the file that doctor read.
type declaredDebezium struct {
	path        string
	service     string
	image       string
	environment map[string]string
}

// errComposeNotFound reports that no compose file exists to read.
var errComposeNotFound = errors.New("no compose file found")

// composeCandidates lists the compose file names that doctor looks for in each
// directory from the working directory up to the file system root.
var composeCandidates = []string{
	filepath.Join("deploy", "compose", "docker-compose.yml"),
	"docker-compose.yml",
	"compose.yaml",
}

// composeDocument holds the part of a compose file that doctor reads.
type composeDocument struct {
	Services map[string]composeService `yaml:"services"`
}

// composeService holds the image and the environment of one compose service.
type composeService struct {
	Image       string    `yaml:"image"`
	Environment yaml.Node `yaml:"environment"`
}

// findComposeFile returns the path of the compose file that declares the
// environment. COMPOSE_FILE wins, because docker compose itself reads it.
func findComposeFile() string {
	if value := os.Getenv("COMPOSE_FILE"); value != "" {
		for _, part := range filepath.SplitList(value) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if info, err := os.Stat(part); err == nil && !info.IsDir() {
				return part
			}
		}
		return ""
	}

	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		for _, candidate := range composeCandidates {
			full := filepath.Join(dir, candidate)
			if info, err := os.Stat(full); err == nil && !info.IsDir() {
				return full
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// loadDeclaredDebezium reads the compose service that declares the Debezium
// environment. It selects the service by environment content and not by name,
// so a renamed service still resolves.
func loadDeclaredDebezium() (declaredDebezium, error) {
	path := findComposeFile()
	if path == "" {
		return declaredDebezium{}, fmt.Errorf("%w: doctor looked for %s and for COMPOSE_FILE", errComposeNotFound, strings.Join(composeCandidates, ", "))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return declaredDebezium{}, fmt.Errorf("read compose file %s: %w", path, err)
	}

	var doc composeDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return declaredDebezium{}, fmt.Errorf("parse compose file %s: %w", path, err)
	}

	names := make([]string, 0, len(doc.Services))
	for serviceName := range doc.Services {
		names = append(names, serviceName)
	}
	sort.Strings(names)

	for _, serviceName := range names {
		service := doc.Services[serviceName]
		environment, err := composeEnvironment(&service.Environment)
		if err != nil {
			return declaredDebezium{}, fmt.Errorf("read the environment of compose service %s in %s: %w", serviceName, path, err)
		}
		if !holdsDebeziumKey(environment) {
			continue
		}
		return declaredDebezium{
			path:        path,
			service:     serviceName,
			image:       service.Image,
			environment: environment,
		}, nil
	}

	return declaredDebezium{}, fmt.Errorf("compose file %s declares no service with a DEBEZIUM_ environment variable", path)
}

// holdsDebeziumKey reports whether an environment declares Debezium Server.
func holdsDebeziumKey(environment map[string]string) bool {
	for key := range environment {
		if strings.HasPrefix(key, "DEBEZIUM_SOURCE_") || strings.HasPrefix(key, "DEBEZIUM_SINK_") {
			return true
		}
	}
	return false
}

// composeEnvironment reads a compose environment block. Compose accepts a
// mapping and a sequence of KEY=VALUE entries, so this reads both forms.
func composeEnvironment(node *yaml.Node) (map[string]string, error) {
	environment := make(map[string]string)
	if node == nil || node.Kind == 0 {
		return environment, nil
	}

	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return environment, nil
	}

	switch node.Kind {
	case yaml.MappingNode:
		var pairs map[string]any
		if err := node.Decode(&pairs); err != nil {
			return nil, fmt.Errorf("decode environment mapping: %w", err)
		}
		for key, value := range pairs {
			if value == nil {
				environment[key] = ""
				continue
			}
			environment[key] = fmt.Sprint(value)
		}
	case yaml.SequenceNode:
		var entries []string
		if err := node.Decode(&entries); err != nil {
			return nil, fmt.Errorf("decode environment sequence: %w", err)
		}
		for _, entry := range entries {
			key, value, _ := strings.Cut(entry, "=")
			environment[strings.TrimSpace(key)] = value
		}
	default:
		return nil, fmt.Errorf("environment is neither a mapping nor a sequence")
	}
	return environment, nil
}

// declaredSourceUnavailable turns a compose read failure into a result. A
// missing file is a skip, because nothing declares the value. A broken file is
// a failure, because a declared source exists and doctor cannot read it.
func declaredSourceUnavailable(name string, err error) CheckResult {
	if errors.Is(err, errComposeNotFound) {
		return CheckResult{
			Name:    name,
			Status:  statusSkip,
			Message: fmt.Sprintf("No compose file declares the Debezium configuration, and Debezium Server serves no endpoint that reports this value: %s", err),
		}
	}
	return CheckResult{
		Name:        name,
		Status:      statusFail,
		Message:     fmt.Sprintf("Cannot read the declared Debezium configuration: %s", err),
		Remediation: "Repair the compose file, or set COMPOSE_FILE to the file that declares the Debezium service.",
	}
}

// checkDebeziumImageTag compares the declared image tag against the tag that
// SDD section 10.1 pins. Debezium Server reports no version over HTTP, so the
// compose file is the source.
func checkDebeziumImageTag(_ context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "debezium-image-tag"
	if !debeziumConfigured(cfg) {
		return CheckResult{Name: name, Status: statusSkip, Message: "Debezium outbox target is not configured."}
	}

	declared, err := loadDeclaredDebezium()
	if err != nil {
		return declaredSourceUnavailable(name, err)
	}

	tag := imageTag(declared.image)
	if tag == "" {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Compose service %s in %s declares the image %q with no tag, so the version floats.", declared.service, declared.path, declared.image),
			Remediation: fmt.Sprintf("Pin the image to quay.io/debezium/server:%s, the tag that the app devops template pins.", requiredDebeziumImageTag),
		}
	}
	if tag != requiredDebeziumImageTag {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Compose service %s in %s pins the Debezium image tag %s, and the app devops template pins %s. The local outbound pipeline is then not the pipeline that production runs.", declared.service, declared.path, tag, requiredDebeziumImageTag),
			Remediation: fmt.Sprintf("Set the image of compose service %s to quay.io/debezium/server:%s, then refresh the digest with deploy/compose/pin-images.sh.", declared.service, requiredDebeziumImageTag),
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: fmt.Sprintf("Compose service %s in %s pins the Debezium image tag %s, which matches the app devops template.", declared.service, declared.path, requiredDebeziumImageTag),
	}
}

// imageTag returns the tag of a container image reference. A digest comes off
// first, because the compose file pins both a tag and a digest.
func imageTag(reference string) string {
	ref := reference
	if index := strings.Index(ref, "@"); index >= 0 {
		ref = ref[:index]
	}
	colon := strings.LastIndex(ref, ":")
	if colon < 0 || colon < strings.LastIndex(ref, "/") {
		return ""
	}
	return ref[colon+1:]
}

// checkHeartbeatDisabled verifies the heartbeat interval of SDD section 10.1.
// A heartbeat with an action query kills the connector and pollutes the run.
func checkHeartbeatDisabled(_ context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "heartbeat-disabled"
	if !debeziumConfigured(cfg) {
		return CheckResult{Name: name, Status: statusSkip, Message: "Debezium outbox target is not configured."}
	}

	declared, err := loadDeclaredDebezium()
	if err != nil {
		return declaredSourceUnavailable(name, err)
	}

	value, present := declared.environment[heartbeatIntervalKey]
	value = strings.TrimSpace(value)

	// Debezium disables the heartbeat when the interval is 0, and 0 is also
	// the Debezium default. An absent key therefore leaves the heartbeat off.
	if !present {
		return CheckResult{
			Name:    name,
			Status:  statusPass,
			Message: fmt.Sprintf("Compose service %s in %s declares no %s, so the Debezium default of 0 applies and the heartbeat stays off.", declared.service, declared.path, heartbeatIntervalKey),
		}
	}
	if value != "0" {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Compose service %s in %s sets %s to %q, so the heartbeat runs. Its action query upserts a fixed-id outbox row, and that update kills the connector or pollutes the run with a payload of {} that fails the conformance check of section 4.11.", declared.service, declared.path, heartbeatIntervalKey, value),
			Remediation: fmt.Sprintf("Set %s to 0 for compose service %s. A Citadel environment is short-lived and a scenario drives its traffic, so the slot advance that the heartbeat provides is not needed.", heartbeatIntervalKey, declared.service),
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: fmt.Sprintf("Compose service %s in %s sets %s to 0, so no action query reaches the outbox.", declared.service, declared.path, heartbeatIntervalKey),
	}
}

// checkRoutingKeySource verifies the routing key configuration of SDD section
// 10.1. Without it every message carries an empty routing key.
func checkRoutingKeySource(_ context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "routing-key-source-set"
	if !debeziumConfigured(cfg) {
		return CheckResult{Name: name, Status: statusSkip, Message: "Debezium outbox target is not configured."}
	}

	declared, err := loadDeclaredDebezium()
	if err != nil {
		return declaredSourceUnavailable(name, err)
	}

	source := strings.TrimSpace(declared.environment[routingKeySourceKey])
	_, legacyPresent := declared.environment[routingKeyFromTopicKey]

	if legacyPresent {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Compose service %s in %s declares %s, and section 10.1 needs it absent. The two settings disagree, and every message then carries an empty routing key.", declared.service, declared.path, routingKeyFromTopicKey),
			Remediation: fmt.Sprintf("Remove %s from compose service %s and keep %s=topic.", routingKeyFromTopicKey, declared.service, routingKeySourceKey),
		}
	}
	if source == "" {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Compose service %s in %s sets no %s, so every message carries an empty routing key and no spy queue binding matches it.", declared.service, declared.path, routingKeySourceKey),
			Remediation: fmt.Sprintf("Set %s=topic for compose service %s. The routing key then equals the exchange name, which is the full aggregatetype such as public.orchestration-engine.Article.v1.", routingKeySourceKey, declared.service),
		}
	}
	if source != "topic" {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Compose service %s in %s sets %s to %q, and section 10.1 needs topic. Every message then carries a routing key that no spy queue binding matches.", declared.service, declared.path, routingKeySourceKey, source),
			Remediation: fmt.Sprintf("Set %s=topic for compose service %s, so the routing key equals the exchange name.", routingKeySourceKey, declared.service),
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: fmt.Sprintf("Compose service %s in %s sets %s=topic and declares no %s, so the routing key equals the exchange name.", declared.service, declared.path, routingKeySourceKey, routingKeyFromTopicKey),
	}
}

// checkAutoCreateRoutingKeyQueues names the queues that autoCreateRoutingKey
// leaves behind, per SDD section 10.1. Each one is bound to nothing.
func checkAutoCreateRoutingKeyQueues(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "auto-create-routing-key-queues"
	if cfg == nil || cfg.Targets.RabbitMQ == nil || cfg.Targets.RabbitMQ.ManagementURL == "" {
		return CheckResult{Name: name, Status: statusSkip, Message: "RabbitMQ management URL is not configured."}
	}

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	queues, err := managementNames(checkCtx, cfg.Targets.RabbitMQ, "/api/queues")
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot read the RabbitMQ queues: %s", err), Remediation: managementAccessRemediation}
	}
	exchanges, err := managementNames(checkCtx, cfg.Targets.RabbitMQ, "/api/exchanges")
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Message: fmt.Sprintf("Cannot read the RabbitMQ exchanges: %s", err), Remediation: managementAccessRemediation}
	}

	exchangeNames := make(map[string]bool, len(exchanges))
	for _, exchange := range exchanges {
		exchangeNames[exchange] = true
	}

	var artifacts []string
	for _, queue := range queues {
		if exchangeNames[queue] {
			artifacts = append(artifacts, queue)
		}
	}

	if len(artifacts) == 0 {
		return CheckResult{Name: name, Status: statusPass, Message: "No queue carries the name of an exchange, so autoCreateRoutingKey left no artifact behind."}
	}

	sort.Strings(artifacts)
	return CheckResult{
		Name:        name,
		Status:      statusPass,
		Message:     fmt.Sprintf("These queues carry the name of an exchange: %s. Each one is an autoCreateRoutingKey artifact. It is bound to nothing, it stays permanently empty, and it is not the queue you look for.", strings.Join(artifacts, ", ")),
		Remediation: "Assert against the spy queue that event-capture binds. Citadel keeps autoCreateRoutingKey so the topology matches production.",
	}
}

// managementAddress joins the configured management URL and a management API
// path.
func managementAddress(target *config.RabbitMQTarget, path string) string {
	return strings.TrimRight(target.ManagementURL, "/") + path
}

// managementRequest builds a GET for one management API path and applies the
// broker credentials. The management plugin rejects an anonymous request with
// status 401, and SDD section 5.1 keeps the credentials in
// targets.rabbitmq.amqpUrl, so a check that sends none can never pass against a
// real broker. A credential written into managementUrl wins, because a
// developer who puts one there means it.
func managementRequest(ctx context.Context, target *config.RabbitMQTarget, path string) (*http.Request, error) {
	addr := managementAddress(target, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", RedactString(addr), err)
	}
	if user, password, ok := target.ManagementAuth(); ok {
		req.SetBasicAuth(user, password)
	}
	return req, nil
}

// managementNames reads the name field of every object at a management API path.
func managementNames(ctx context.Context, target *config.RabbitMQTarget, path string) ([]string, error) {
	addr := RedactString(managementAddress(target, path))
	req, err := managementRequest(ctx, target, path)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A transport error carries the address, and net/url masks the
		// password of a URL that holds one before it prints it.
		return nil, fmt.Errorf("get the management API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s returned status %d", addr, resp.StatusCode)
	}
	var objects []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&objects); err != nil {
		return nil, fmt.Errorf("decode the response of %s: %w", addr, err)
	}
	names := make([]string, 0, len(objects))
	for _, object := range objects {
		names = append(names, object.Name)
	}
	return names, nil
}

// openDatabase opens a database target and verifies it with a ping.
func openDatabase(ctx context.Context, target *config.DatabaseTarget) (*sql.DB, error) {
	openCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	handle, err := db.Open(openCtx, target.Driver, target.DSN)
	if err != nil {
		return nil, err
	}
	return handle, nil
}

// databaseUnreachable turns a connection failure into a check result.
func databaseUnreachable(name string, err error) CheckResult {
	return CheckResult{
		Name:        name,
		Status:      statusFail,
		Message:     fmt.Sprintf("Cannot connect to the app database: %s", RedactString(err.Error())),
		Remediation: "Start the app database, then verify targets.app-db.driver is postgres or sqlserver and targets.app-db.dsn is correct.",
	}
}

// targetDialect returns the dialect of a database target. An unsupported
// driver name resolves to Postgres, because db.Open then names the driver
// itself and one message for one cause is enough.
func targetDialect(target *config.DatabaseTarget) db.Dialect {
	dialect, err := db.DialectOf(target.Driver)
	if err != nil {
		return db.DialectPostgres
	}
	return dialect
}

// sqlServerTarget returns the first database target whose driver is SQL Server,
// with the name that the configuration gives it. SDD section 10.2 keeps the
// shadow on Postgres, so in practice this is the app database.
func sqlServerTarget(cfg *config.CitadelConfig) (string, *config.DatabaseTarget) {
	if cfg == nil {
		return "", nil
	}
	for _, candidate := range databaseTargets(cfg) {
		if targetDialect(candidate.target) == db.DialectSQLServer {
			return candidate.name, candidate.target
		}
	}
	return "", nil
}

// postgresOnlyCheck returns the skip that a Postgres-only check reports for a
// SQL Server target. A skip states the reason, and a pass would claim evidence
// that no query produced.
func postgresOnlyCheck(name, object string) CheckResult {
	return CheckResult{
		Name:    name,
		Status:  statusSkip,
		Message: fmt.Sprintf("The app database driver is sqlserver, and %s belongs to Postgres. SQL Server carries the capture job of section 10.2 in place of it.", object),
	}
}

// checkInboxTable verifies the inbox table and every configured column, per
// SDD section 8.4. A wrong column name otherwise surfaces as a SQL error
// mid-scenario.
func checkInboxTable(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "inbox-table-exists"
	if cfg == nil || cfg.Targets.AppDB == nil || cfg.Targets.AppDB.DSN == "" {
		return CheckResult{Name: name, Status: statusSkip, Message: "App database target is not configured."}
	}
	if cfg.Targets.AppDB.Inbox == nil {
		return CheckResult{Name: name, Status: statusSkip, Message: "No inbox configuration on the app-db target."}
	}

	handle, err := openDatabase(ctx, cfg.Targets.AppDB)
	if err != nil {
		return databaseUnreachable(name, err)
	}
	defer handle.Close()

	return inboxTableResult(ctx, handle, cfg.Targets.AppDB.Inbox, targetDialect(cfg.Targets.AppDB))
}

// inboxColumnQueryFor returns the information_schema query for a dialect. Both
// engines hold the view, and only the placeholder marker differs.
func inboxColumnQueryFor(dialect db.Dialect) string {
	if dialect == db.DialectSQLServer {
		return inboxColumnQuerySQLServer
	}
	return inboxColumnQuery
}

// inboxTableResult compares the configured inbox names against the names that
// the database holds.
func inboxTableResult(ctx context.Context, handle *sql.DB, inbox *config.InboxConfig, dialect db.Dialect) CheckResult {
	const name = "inbox-table-exists"

	schema, table := "", inbox.Table
	if index := strings.LastIndex(inbox.Table, "."); index >= 0 {
		schema = inbox.Table[:index]
		table = inbox.Table[index+1:]
	}

	expected := []string{
		inbox.MessageIDColumn,
		inbox.ProcessedAtColumn,
		inbox.RetryCountColumn,
		inbox.LastErrorColumn,
	}
	var wanted []string
	for _, column := range expected {
		if strings.TrimSpace(column) != "" {
			wanted = append(wanted, column)
		}
	}

	queryCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	rows, err := handle.QueryContext(queryCtx, inboxColumnQueryFor(dialect), table)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read the columns of the inbox table %s: %s", inbox.Table, err),
			Remediation: "Verify the app database accepts the configured DSN and that the role can read information_schema.",
		}
	}
	defer rows.Close()

	var actual []string
	present := make(map[string]bool)
	for rows.Next() {
		var rowSchema, column string
		if err := rows.Scan(&rowSchema, &column); err != nil {
			return CheckResult{
				Name:        name,
				Status:      statusFail,
				Message:     fmt.Sprintf("Cannot read a column name of the inbox table %s: %s", inbox.Table, err),
				Remediation: "Verify the app database accepts the configured DSN.",
			}
		}
		if schema != "" && rowSchema != schema {
			continue
		}
		actual = append(actual, column)
		present[column] = true
	}
	if err := rows.Err(); err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read the columns of the inbox table %s: %s", inbox.Table, err),
			Remediation: "Verify the app database accepts the configured DSN.",
		}
	}
	sort.Strings(actual)

	if len(actual) == 0 {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("The app database holds no table %s. Expected columns: %s. Actual columns: none.", inbox.Table, strings.Join(wanted, ", ")),
			Remediation: fmt.Sprintf("Create the table %s with the columns %s, or set targets.app-db.inbox.table to the name the service uses.", inbox.Table, strings.Join(wanted, ", ")),
		}
	}

	var missing []string
	for _, column := range wanted {
		if !present[column] {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("The inbox table %s misses the configured columns %s. Expected columns: %s. Actual columns: %s.", inbox.Table, strings.Join(missing, ", "), strings.Join(wanted, ", "), strings.Join(actual, ", ")),
			Remediation: fmt.Sprintf("Set the column names under targets.app-db.inbox to the names the table holds, or add the columns %s to %s.", strings.Join(missing, ", "), inbox.Table),
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: fmt.Sprintf("The inbox table %s holds every configured column: %s.", inbox.Table, strings.Join(wanted, ", ")),
	}
}

// checkWalLevel reads wal_level from the app database. Logical decoding needs
// the logical level, and SDD section 10.2 sets it with a command override.
func checkWalLevel(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "postgres-wal-level"
	if cfg == nil || cfg.Targets.AppDB == nil || cfg.Targets.AppDB.DSN == "" {
		return CheckResult{Name: name, Status: statusSkip, Message: "App database target is not configured."}
	}
	if targetDialect(cfg.Targets.AppDB) == db.DialectSQLServer {
		return postgresOnlyCheck(name, "wal_level")
	}

	handle, err := openDatabase(ctx, cfg.Targets.AppDB)
	if err != nil {
		return databaseUnreachable(name, err)
	}
	defer handle.Close()

	return walLevelResult(ctx, handle)
}

// walLevelResult compares the wal_level of a live database against logical.
func walLevelResult(ctx context.Context, handle *sql.DB) CheckResult {
	const name = "postgres-wal-level"

	queryCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	var level string
	if err := handle.QueryRowContext(queryCtx, walLevelQuery).Scan(&level); err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read wal_level from the app database: %s", err),
			Remediation: walLevelRemediation,
		}
	}

	level = strings.TrimSpace(level)
	if !strings.EqualFold(level, "logical") {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("The app database reports wal_level=%s, and logical decoding needs wal_level=logical. Debezium cannot create a replication slot at this level.", level),
			Remediation: walLevelRemediation,
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: "The app database reports wal_level=logical.",
	}
}

// checkStaleReplicationSlot looks for a replication slot that outlived its
// Debezium. SDD section 4.9 states that such a slot resumes from its old LSN,
// so the environment never re-snapshots and the reset was not a reset.
func checkStaleReplicationSlot(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "no-stale-replication-slot"
	if cfg == nil || cfg.Targets.AppDB == nil || cfg.Targets.AppDB.DSN == "" {
		return CheckResult{Name: name, Status: statusSkip, Message: "App database target is not configured."}
	}
	if targetDialect(cfg.Targets.AppDB) == db.DialectSQLServer {
		return postgresOnlyCheck(name, "a replication slot")
	}

	handle, err := openDatabase(ctx, cfg.Targets.AppDB)
	if err != nil {
		return databaseUnreachable(name, err)
	}
	defer handle.Close()

	return staleReplicationSlotResult(ctx, handle)
}

// staleReplicationSlotResult reads pg_replication_slots. An inactive slot has
// no walsender attached, so it is a leftover of an earlier run. An active slot
// belongs to the Debezium that runs now.
func staleReplicationSlotResult(ctx context.Context, handle *sql.DB) CheckResult {
	const name = "no-stale-replication-slot"

	queryCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	rows, err := handle.QueryContext(queryCtx, replicationSlotQuery)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read pg_replication_slots on the app database: %s", err),
			Remediation: "Grant the configured role permission to read pg_replication_slots, then run doctor again.",
		}
	}
	defer rows.Close()

	var stale, active []string
	for rows.Next() {
		var slotName string
		var slotActive bool
		if err := rows.Scan(&slotName, &slotActive); err != nil {
			return CheckResult{
				Name:        name,
				Status:      statusFail,
				Message:     fmt.Sprintf("Cannot read a row of pg_replication_slots: %s", err),
				Remediation: "Verify the app database accepts the configured DSN.",
			}
		}
		if slotActive {
			active = append(active, slotName)
			continue
		}
		stale = append(stale, slotName)
	}
	if err := rows.Err(); err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read pg_replication_slots on the app database: %s", err),
			Remediation: "Verify the app database accepts the configured DSN.",
		}
	}

	if len(stale) > 0 {
		sort.Strings(stale)
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("These replication slots survive with active=false: %s. Each one holds an old LSN, so Debezium resumes from it and never re-snapshots. A reset does not remove a slot, because the slot lives in the database.", strings.Join(stale, ", ")),
			Remediation: staleSlotRemediation,
		}
	}

	if len(active) > 0 {
		sort.Strings(active)
		return CheckResult{
			Name:    name,
			Status:  statusPass,
			Message: fmt.Sprintf("No stale slot exists. These slots are active and belong to the Debezium that runs now: %s.", strings.Join(active, ", ")),
		}
	}

	return CheckResult{Name: name, Status: statusPass, Message: "The app database holds no replication slot."}
}

// noSQLServerTarget returns the skip that a SQL Server check reports when the
// configuration names no SQL Server target. The message states the fact that
// doctor read, and it does not assert a fact about a target it never inspected.
func noSQLServerTarget(name string) CheckResult {
	return CheckResult{
		Name:    name,
		Status:  statusSkip,
		Message: "No database target names the sqlserver driver, so this check does not apply. SQL Server CDC sits behind the cdc-sqlserver profile.",
	}
}

// sqlServerUnreachable turns a connection failure of a SQL Server check into a
// result. The message names the target and the redacted address.
func sqlServerUnreachable(name, targetName string, target *config.DatabaseTarget, err error) CheckResult {
	return CheckResult{
		Name:        name,
		Status:      statusFail,
		Message:     fmt.Sprintf("Cannot connect to the SQL Server target %s at %s: %s", targetName, RedactString(target.DSN), RedactString(err.Error())),
		Remediation: fmt.Sprintf("Start the cdc-sqlserver compose profile with 'docker compose --profile cdc-sqlserver up -d', then verify targets.%s.dsn in the configuration.", targetName),
	}
}

// checkSQLAgent covers the SQL Server row of SDD section 12.3. SDD section 10.2
// enables the Agent, and the capture job is an Agent job, so CDC emits no
// change while the Agent is down.
func checkSQLAgent(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "sql-agent-running"

	targetName, target := sqlServerTarget(cfg)
	if target == nil {
		return noSQLServerTarget(name)
	}

	handle, err := openDatabase(ctx, target)
	if err != nil {
		return sqlServerUnreachable(name, targetName, target, err)
	}
	defer handle.Close()

	return sqlAgentResult(ctx, handle, targetName)
}

// sqlAgentResult reads sys.dm_server_services, which reports the state of every
// SQL Server service on the instance.
func sqlAgentResult(ctx context.Context, handle *sql.DB, targetName string) CheckResult {
	const name = "sql-agent-running"

	queryCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	rows, err := handle.QueryContext(queryCtx, sqlAgentQuery)
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read sys.dm_server_services on the SQL Server target %s: %s", targetName, RedactString(err.Error())),
			Remediation: fmt.Sprintf("Grant VIEW SERVER STATE to the role in targets.%s.dsn, then run doctor again. Without the state doctor cannot prove the Agent runs, and CDC emits no change without it.", targetName),
		}
	}
	defer rows.Close()

	var states []string
	live := false
	for rows.Next() {
		var service, state string
		if err := rows.Scan(&service, &state); err != nil {
			return CheckResult{
				Name:        name,
				Status:      statusFail,
				Message:     fmt.Sprintf("Cannot read a row of sys.dm_server_services on the SQL Server target %s: %s", targetName, RedactString(err.Error())),
				Remediation: sqlAgentRemediation,
			}
		}
		states = append(states, fmt.Sprintf("%s=%s", service, state))
		if strings.EqualFold(state, "Running") {
			live = true
		}
	}
	if err := rows.Err(); err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read sys.dm_server_services on the SQL Server target %s: %s", targetName, RedactString(err.Error())),
			Remediation: sqlAgentRemediation,
		}
	}
	sort.Strings(states)

	if len(states) == 0 {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("The SQL Server target %s reports no SQL Server Agent service at all, so CDC never emits a change.", targetName),
			Remediation: sqlAgentRemediation,
		}
	}
	if !live {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("The SQL Server target %s reports the Agent state %s, and CDC needs Running. The capture job is an Agent job, so CDC never emits a change at this state.", targetName, strings.Join(states, ", ")),
			Remediation: sqlAgentRemediation,
		}
	}

	return CheckResult{
		Name:    name,
		Status:  statusPass,
		Message: fmt.Sprintf("The SQL Server target %s reports the Agent state %s.", targetName, strings.Join(states, ", ")),
	}
}

// checkCaptureJobPollInterval covers the capture-job row of SDD section 12.3.
// SDD section 10.2 needs a reduced interval. msdb.dbo.cdc_jobs holds the
// recorded value, and the live job keeps its old value until it stops and
// starts, so a pass names the restart that the value depends on.
func checkCaptureJobPollInterval(ctx context.Context, cfg *config.CitadelConfig) CheckResult {
	const name = "capture-job-poll-interval"

	targetName, target := sqlServerTarget(cfg)
	if target == nil {
		return noSQLServerTarget(name)
	}

	handle, err := openDatabase(ctx, target)
	if err != nil {
		return sqlServerUnreachable(name, targetName, target, err)
	}
	defer handle.Close()

	return captureJobPollIntervalResult(ctx, handle, targetName)
}

// captureJobPollIntervalResult reads the recorded poll interval of the capture
// job. The default of SQL Server proves that nobody reduced the value.
func captureJobPollIntervalResult(ctx context.Context, handle *sql.DB, targetName string) CheckResult {
	const name = "capture-job-poll-interval"

	queryCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	var interval int
	err := handle.QueryRowContext(queryCtx, captureJobQuery).Scan(&interval)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("The SQL Server target %s holds no capture job in msdb.dbo.cdc_jobs, so CDC is not enabled and no poll interval applies.", targetName),
			Remediation: "Enable CDC on the database with 'EXEC sys.sp_cdc_enable_db', enable it on each table with 'EXEC sys.sp_cdc_enable_table', then reduce the poll interval. " + captureJobRestartRemediation,
		}
	}
	if err != nil {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("Cannot read msdb.dbo.cdc_jobs on the SQL Server target %s: %s", targetName, RedactString(err.Error())),
			Remediation: fmt.Sprintf("Grant the role in targets.%s.dsn read permission on msdb.dbo.cdc_jobs, then run doctor again. ", targetName) + captureJobRestartRemediation,
		}
	}

	if interval >= defaultCaptureJobPollInterval {
		return CheckResult{
			Name:        name,
			Status:      statusFail,
			Message:     fmt.Sprintf("The capture job of the SQL Server target %s records the poll interval %d seconds, and the SQL Server default is %d seconds. Section 10.2 needs a reduced value, so a scenario waits a full interval for every change.", targetName, interval, defaultCaptureJobPollInterval),
			Remediation: captureJobRestartRemediation,
		}
	}

	return CheckResult{
		Name:   name,
		Status: statusPass,
		Message: fmt.Sprintf("The capture job of the SQL Server target %s records the poll interval %d seconds, below the SQL Server default of %d seconds. The live job takes a recorded value only after 'EXEC sys.sp_cdc_stop_job' and 'EXEC sys.sp_cdc_start_job', so restart the job after any later change.",
			targetName, interval, defaultCaptureJobPollInterval),
	}
}
