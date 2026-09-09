package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/andersonten/simcitadel/internal/expect"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// ProbeResult holds the outcome of a single probe check.
type ProbeResult struct {
	// Source names the probe source of SDD section 6.5 that produced the
	// result. An expectation key the table scopes to one source reads it.
	Source     string
	Response   any
	StatusCode int
	RawBody    []byte
	Err        error
}

// ExecuteProbe runs a single probe against its source and returns the result.
// ExecuteProbe does not retry; use ExecuteWithRetry for that.
func ExecuteProbe(ctx context.Context, step *scenario.Step, rc *RunContext) (*ProbeResult, error) {
	source, read := resolveProbeSource(step)
	if read == nil {
		return nil, fmt.Errorf("probe step %q has no source configured", step.Name)
	}
	result, err := read(ctx, step, rc)
	if err != nil {
		return nil, err
	}
	// The source travels with the result, because SDD section 6.5 scopes some
	// expectation keys to one source and the body alone does not say which
	// source produced it.
	result.Source = source
	return result, nil
}

// resolveProbeSource returns the source of SDD section 6.5 that the step reads
// and the function that reads it. A step with no source returns a nil function.
func resolveProbeSource(step *scenario.Step) (string, func(context.Context, *scenario.Step, *RunContext) (*ProbeResult, error)) {
	switch {
	case step.GraphQL != nil:
		return expect.SourceGraphQL, probeGraphQL
	case step.HTTP != nil:
		return expect.SourceHTTP, probeHTTP
	case step.Captured != nil:
		return expect.SourceCaptured, probeCaptured
	case step.Journal != nil:
		return expect.SourceJournal, probeJournal
	case step.SQL != nil:
		return expect.SourceSQL, probeSQL
	}
	return "", nil
}

// probeSQL is the sql source of SDD section 6.5. It returns the rows, so the
// rows, columns and notNull expectations all read the same result.
//
// It is also the read of a sql/query step, which writes its query at the
// step level as well as under sql:, so both shapes reach one place.
//
// A statement that did not answer travels as a probe failure and not as an
// error, so the retry loop can tell the window closing from the database going
// away. SDD section 4.6 reports the first as an assertion failure.
func probeSQL(ctx context.Context, step *scenario.Step, rc *RunContext) (*ProbeResult, error) {
	rows, err := ExecuteSQLQuery(ctx, step, rc)
	if err != nil {
		return &ProbeResult{Source: expect.SourceSQL, Err: err}, nil
	}
	return &ProbeResult{Source: expect.SourceSQL, Response: toAnyList(rows)}, nil
}

// toAnyList normalises a row set to []any, so the count and row expectations
// see the same shape whatever the scanner returned.
func toAnyList(rows any) any {
	switch v := rows.(type) {
	case []any:
		return v
	case []map[string]any:
		out := make([]any, len(v))
		for i, row := range v {
			out[i] = row
		}
		return out
	}
	return rows
}

func probeGraphQL(ctx context.Context, step *scenario.Step, rc *RunContext) (*ProbeResult, error) {
	target, baseURL, err := resolveProbeTarget(rc, step.Target)
	if err != nil {
		return nil, fmt.Errorf("probe graphql: %w", err)
	}
	gqlPath := graphQLPath(rc, target)

	query := step.GraphQL.Query
	if query == "" && step.GraphQL.QueryFile != "" {
		path := rc.Config.QueryPath(step.GraphQL.QueryFile)
		rendered, err := readRenderedContent(path, rc)
		if err != nil {
			return nil, fmt.Errorf("probe graphql query file: %w", err)
		}
		query = rendered
	}

	body := map[string]any{
		"query": query,
	}
	if len(step.GraphQL.Variables) > 0 {
		body["variables"] = step.GraphQL.Variables
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("probe graphql marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+gqlPath, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("probe graphql create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if rc.BearerToken != "" && targetRequiresAuth(rc, target) {
		req.Header.Set("Authorization", "Bearer "+rc.BearerToken)
	}

	resp, err := rc.HTTPClient.Do(req)
	if err != nil {
		return &ProbeResult{Err: err}, nil
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("probe graphql read response: %w", err)
	}

	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		parsed = string(rawBody)
	}

	return &ProbeResult{
		Response:   parsed,
		StatusCode: resp.StatusCode,
		RawBody:    rawBody,
	}, nil
}

func probeHTTP(ctx context.Context, step *scenario.Step, rc *RunContext) (*ProbeResult, error) {
	target, baseURL, err := resolveProbeTarget(rc, step.Target)
	if err != nil {
		return nil, fmt.Errorf("probe http: %w", err)
	}

	method := step.HTTP.Method
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader io.Reader
	if step.HTTP.Body != nil {
		payload, err := json.Marshal(step.HTTP.Body)
		if err != nil {
			return nil, fmt.Errorf("probe http marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	} else if step.HTTP.BodyFile != "" {
		path := rc.Config.PayloadPath(step.HTTP.BodyFile)
		rendered, err := readRenderedContent(path, rc)
		if err != nil {
			return nil, fmt.Errorf("probe http body file: %w", err)
		}
		bodyReader = strings.NewReader(rendered)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+step.HTTP.Path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("probe http create request: %w", err)
	}
	for k, v := range step.HTTP.Headers {
		req.Header.Set(k, v)
	}
	if step.HTTP.Body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if rc.BearerToken != "" && targetRequiresAuth(rc, target) {
		req.Header.Set("Authorization", "Bearer "+rc.BearerToken)
	}

	resp, err := rc.HTTPClient.Do(req)
	if err != nil {
		return &ProbeResult{Err: err}, nil
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("probe http read response: %w", err)
	}

	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		parsed = string(rawBody)
	}

	return &ProbeResult{
		Response:   parsed,
		StatusCode: resp.StatusCode,
		RawBody:    rawBody,
	}, nil
}

func probeCaptured(ctx context.Context, step *scenario.Step, rc *RunContext) (*ProbeResult, error) {
	if rc.Config.Targets.EventCapture == nil {
		return nil, fmt.Errorf("probe captured: event-capture target is not configured")
	}
	captureURL := rc.Config.Targets.EventCapture.URL

	params := url.Values{}
	if step.Captured.Exchange != "" {
		params.Set("exchange", step.Captured.Exchange)
	}
	if step.Captured.RoutingKey != "" {
		params.Set("routingKey", step.Captured.RoutingKey)
	}
	if step.Captured.RoutingKeyPattern != "" {
		params.Set("routingKeyPattern", step.Captured.RoutingKeyPattern)
	}
	if step.Captured.EventType != "" {
		params.Set("eventType", step.Captured.EventType)
	}
	if step.Captured.EventTypePattern != "" {
		params.Set("eventTypePattern", step.Captured.EventTypePattern)
	}
	if step.Captured.RunID != "" {
		params.Set("runId", step.Captured.RunID)
	} else if rc.RunID != "" {
		params.Set("runId", rc.RunID)
	}
	if step.Captured.JSONPath != "" {
		params.Set("jsonPath", step.Captured.JSONPath)
	}
	for k, v := range step.Captured.Headers {
		params.Set("header."+k, v)
	}

	reqURL := captureURL + "/captured?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("probe captured create request: %w", err)
	}

	resp, err := rc.HTTPClient.Do(req)
	if err != nil {
		return &ProbeResult{Err: err}, nil
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("probe captured read response: %w", err)
	}

	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		parsed = string(rawBody)
	}

	return &ProbeResult{
		Response:   parsed,
		StatusCode: resp.StatusCode,
		RawBody:    rawBody,
	}, nil
}

// defaultProbeTarget is where a probe goes when the step names no target.
const defaultProbeTarget = "app"

// resolveProbeTarget returns the target a probe reads and its base address. SDD
// section 6.5 makes target: the destination of the probe, so a step that names
// another target must reach that target and not the app. A target the config
// does not hold, or holds with no address, is an error that names it, because a
// probe that quietly goes somewhere else proves nothing.
func resolveProbeTarget(rc *RunContext, target string) (string, string, error) {
	name := target
	if name == "" {
		name = defaultProbeTarget
	}

	t := rc.Config.Targets
	var address string
	switch name {
	case "app":
		if t.App != nil {
			address = t.App.URL
		}
	case "wiremock":
		if t.WireMock != nil {
			address = t.WireMock.URL
		}
	case "event-trigger":
		if t.EventTrigger != nil {
			address = t.EventTrigger.URL
		}
	case "event-capture":
		if t.EventCapture != nil {
			address = t.EventCapture.URL
		}
	case "debezium-outbox":
		if t.DebeziumOutbox != nil {
			address = t.DebeziumOutbox.URL
		}
	default:
		return "", "", fmt.Errorf("target %q is not an HTTP target of the configuration", name)
	}

	if address == "" {
		return "", "", fmt.Errorf("target %q holds no address", name)
	}
	return name, address, nil
}

// graphQLPath returns the GraphQL endpoint path of a target. Only the app
// target configures its own path, so every other target takes the default.
func graphQLPath(rc *RunContext, target string) string {
	if target == "app" && rc.Config.Targets.App != nil && rc.Config.Targets.App.GraphQLPath != "" {
		return rc.Config.Targets.App.GraphQLPath
	}
	return "/graphql"
}

// targetRequiresAuth reports whether a target asks for the bearer token of SDD
// section 9. The app target is the only one that names an auth client, so no
// other target receives the token.
func targetRequiresAuth(rc *RunContext, target string) bool {
	if rc.Config.Targets.App != nil && target == "app" {
		return rc.Config.Targets.App.Auth != ""
	}
	return false
}

// journalFindPath is the WireMock admin path of a criteria query against the
// request journal. WireMock ignores a method parameter and a urlPath parameter
// on GET /__admin/requests, so a journal query travels as a request pattern in
// the body of a POST to this path. The answer holds the matched requests, so one
// call serves the match list and the match count.
const journalFindPath = "/requests/find"

// journalFindResponse is the answer of the WireMock criteria query. WireMock
// reports that the journal is off rather than an error, and a probe against a
// journal that is off proves nothing.
type journalFindResponse struct {
	Requests               []any `json:"requests"`
	RequestJournalDisabled bool  `json:"requestJournalDisabled"`
}

// probeJournal is the journal source of SDD section 6.5. It asks WireMock for
// the journal entries that match the method and the urlPath of the step, and it
// returns those entries as a list. The length of the list is the match count, so
// count and countAtLeast read the filtered set and not the whole journal.
func probeJournal(ctx context.Context, step *scenario.Step, rc *RunContext) (*ProbeResult, error) {
	if rc.Config.Targets.WireMock == nil {
		return nil, fmt.Errorf("probe journal: wiremock target is not configured")
	}

	pattern, err := json.Marshal(journalRequestPattern(step.Journal))
	if err != nil {
		return nil, fmt.Errorf("probe journal marshal request pattern: %w", err)
	}

	reqURL := rc.Config.Targets.WireMock.AdminBase() + journalFindPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(pattern))
	if err != nil {
		return nil, fmt.Errorf("probe journal create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := rc.HTTPClient.Do(req)
	if err != nil {
		return &ProbeResult{Err: err}, nil
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("probe journal read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("probe journal: wiremock answered %d for %s: %s",
			resp.StatusCode, journalFindPath, strings.TrimSpace(string(rawBody)))
	}

	var found journalFindResponse
	if err := json.Unmarshal(rawBody, &found); err != nil {
		return nil, fmt.Errorf("probe journal decode response: %w", err)
	}
	if found.RequestJournalDisabled {
		return nil, fmt.Errorf("probe journal: the wiremock request journal is off, so a journal probe proves nothing")
	}

	// An empty match list stays a list, so a count expectation reads zero and
	// reports a real count instead of a shape error.
	matches := found.Requests
	if matches == nil {
		matches = []any{}
	}

	return &ProbeResult{
		Response:   matches,
		StatusCode: resp.StatusCode,
		RawBody:    rawBody,
	}, nil
}

// journalRequestPattern returns the WireMock request pattern of a journal
// query. WireMock matches method in upper case only, so a lower case method of
// the scenario reaches WireMock in upper case. WireMock matches urlPath as the
// exact path and it ignores the query string, which is the match SDD section
// 5.1 asks for.
func journalRequestPattern(q *scenario.JournalQuery) map[string]string {
	pattern := map[string]string{}
	if q == nil {
		return pattern
	}
	if q.Method != "" {
		pattern["method"] = strings.ToUpper(q.Method)
	}
	if q.URLPath != "" {
		pattern["urlPath"] = q.URLPath
	}
	return pattern
}
