package steps

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	// The local variable db holds a connection in several functions here, so the
	// package needs a name of its own.
	citadeldb "github.com/andersonten/simcitadel/internal/db"
	"github.com/andersonten/simcitadel/internal/expect"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// executeNotCaptured verifies that no message matches the filter criteria.
// It queries the capture service twice, separated by the quiet period.
// The step passes only when both queries return zero matches.
func executeNotCaptured(ctx context.Context, step *scenario.Step, rc *RunContext) (*StepResult, error) {
	if rc.Config.Targets.EventCapture == nil {
		return nil, fmt.Errorf("not-captured: event-capture target is not configured")
	}
	captureURL := rc.Config.Targets.EventCapture.URL

	// The quiet period is read before the first query, so a value the parser
	// cannot read fails the step rather than shortening the window it names.
	quietPeriod, err := resolveQuietPeriod(step, rc)
	if err != nil {
		return nil, fmt.Errorf("not-captured: %w", err)
	}

	params, applied := buildNotCapturedParams(step, rc)
	reqURL := captureURL + "/captured?" + params.Encode()

	matches, err := queryCaptured(ctx, rc.HTTPClient, reqURL)
	if err != nil {
		return nil, fmt.Errorf("not-captured first query: %w", err)
	}
	if len(matches) > 0 {
		return notCapturedFailure(matches, "first", applied), nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(quietPeriod):
	}

	matches, err = queryCaptured(ctx, rc.HTTPClient, reqURL)
	if err != nil {
		return nil, fmt.Errorf("not-captured second query: %w", err)
	}
	if len(matches) > 0 {
		return notCapturedFailure(matches, "second", applied), nil
	}

	return &StepResult{
		Pass: true,
		Expect: &expect.Result{
			Pass:    true,
			Message: fmt.Sprintf("not-captured: no message matched (selectors: %s)", applied),
		},
	}, nil
}

// notCapturedFailure reports a match. SDD section 8.3 wants the matched message
// and the selector that found it, so the run output shows what was checked.
func notCapturedFailure(matches []any, query, applied string) *StepResult {
	err := fmt.Errorf("not-captured: found %d matching messages on %s query (selectors: %s)",
		len(matches), query, applied)
	return &StepResult{
		Pass: false,
		Err:  err,
		Expect: &expect.Result{
			Pass:    false,
			Actual:  matches[0],
			Message: err.Error(),
		},
	}
}

// buildNotCapturedParams builds the capture query of SDD section 8.2 and the
// description of the selectors it applies.
//
// A selector arrives either on the step or under captured:, because validate
// accepts both shapes, so both are read here. Every selector the capture query
// engine implements is forwarded: one that stays behind would widen the query
// and let the assertion pass while it checked something else.
func buildNotCapturedParams(step *scenario.Step, rc *RunContext) (url.Values, string) {
	captured := step.Captured
	if captured == nil {
		captured = &scenario.CapturedQuery{}
	}

	selectors := []struct {
		name  string
		value string
	}{
		{"exchange", firstNonEmpty(step.Exchange, captured.Exchange)},
		{"routingKey", firstNonEmpty(step.RoutingKey, captured.RoutingKey)},
		{"routingKeyPattern", captured.RoutingKeyPattern},
		{"eventType", firstNonEmpty(step.EventType, captured.EventType)},
		{"eventTypePattern", firstNonEmpty(step.EventTypePattern, captured.EventTypePattern)},
		{"jsonPath", captured.JSONPath},
	}

	headers := make(map[string]string, len(step.HeaderFilter)+len(captured.Headers))
	for k, v := range captured.Headers {
		headers[k] = v
	}
	for k, v := range step.HeaderFilter {
		headers[k] = v
	}
	for _, name := range sortedKeys(headers) {
		selectors = append(selectors, struct {
			name  string
			value string
		}{"header." + name, headers[name]})
	}

	params := url.Values{}
	params.Set("since", rc.RunStart.Format(time.RFC3339))
	if rc.RunID != "" {
		params.Set("runId", rc.RunID)
	}

	applied := make([]string, 0, len(selectors))
	for _, s := range selectors {
		if s.value == "" {
			continue
		}
		params.Set(s.name, s.value)
		applied = append(applied, s.name+"="+s.value)
	}
	if len(applied) == 0 {
		return params, "none"
	}
	return params, strings.Join(applied, ", ")
}

// firstNonEmpty returns the first value that is set. A selector written on the
// step wins over the same selector under captured:.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// sortedKeys returns the map keys in order, so one step always produces one
// query and one message.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveQuietPeriod returns the quiet period of SDD section 8.3. The step
// overrides the config default, and a step value the parser cannot read is an
// error. A silent fall back to the default shortens the window the author asked
// for, and a message that arrives after the shorter window then makes the
// assertion report a pass.
func resolveQuietPeriod(step *scenario.Step, rc *RunContext) (time.Duration, error) {
	if step.QuietPeriod != "" {
		d, err := time.ParseDuration(step.QuietPeriod)
		if err != nil {
			return 0, fmt.Errorf("quietPeriod %q is not a duration: %w", step.QuietPeriod, err)
		}
		return d, nil
	}
	d := rc.Config.Defaults.QuietPeriod.Duration()
	if d == 0 {
		return config.DefaultQuietPeriod, nil
	}
	return d, nil
}

func queryCaptured(ctx context.Context, client *http.Client, reqURL string) ([]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var messages []any
	if err := json.Unmarshal(body, &messages); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return messages, nil
}

// executeRepublish re-sends a previously captured message with its original
// CloudEvents ID and body. It reads the message from the binding stored by a
// prior step.
func executeRepublish(ctx context.Context, step *scenario.Step, rc *RunContext) (*StepResult, error) {
	if step.Ref == "" {
		return nil, fmt.Errorf("republish: ref is required")
	}

	binding, ok := rc.Bindings[step.Ref]
	if !ok {
		return nil, fmt.Errorf("republish: binding %q does not exist", step.Ref)
	}

	raw, err := json.Marshal(binding)
	if err != nil {
		return nil, fmt.Errorf("republish: marshal binding %q: %w", step.Ref, err)
	}

	var msg struct {
		Body        json.RawMessage `json:"body"`
		CloudEvents struct {
			ID string `json:"id"`
		} `json:"cloudEvents"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("republish: parse binding %q: %w", step.Ref, err)
	}

	var data any
	if len(msg.Body) > 0 {
		if jsonErr := json.Unmarshal(msg.Body, &data); jsonErr != nil {
			data = string(msg.Body)
		}
	}
	if data == nil {
		return nil, fmt.Errorf("republish: binding %q has no message body", step.Ref)
	}

	if rc.Config.Targets.EventTrigger == nil {
		return nil, fmt.Errorf("republish: event-trigger target is not configured")
	}
	triggerURL := rc.Config.Targets.EventTrigger.URL

	payload := map[string]any{
		"exchange":   step.Exchange,
		"routingKey": step.RoutingKey,
		"event":      data,
	}
	if msg.CloudEvents.ID != "" {
		payload["eventId"] = msg.CloudEvents.ID
	}
	if step.Delivery != "" {
		payload["delivery"] = step.Delivery
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("republish: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, triggerURL+"/publish", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("republish: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", rc.RunID)

	resp, err := rc.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("republish: send request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("republish: event-trigger returned status %d", resp.StatusCode)
	}

	return &StepResult{Pass: true}, nil
}

// probeInboxRows is the read of an inbox/assert step. It runs the canonical
// query of SDD section 8.4 and returns the rows, so rows, columns and notNull
// all read the same result.
//
// A configuration problem is an error and never repeats, because no wait fixes
// it. A statement that did not answer travels as a probe failure, so the retry
// loop can tell the window closing from the database going away.
func probeInboxRows(ctx context.Context, step *scenario.Step, rc *RunContext) (*ProbeResult, error) {
	handle, query, messageID, err := inboxQuery(step, rc)
	if err != nil {
		return nil, err
	}
	rows, err := queryInboxRows(ctx, handle, query, messageID)
	if err != nil {
		return &ProbeResult{
			Source: expect.SourceSQL,
			Err:    fmt.Errorf("inbox/assert: query: %w", err),
		}, nil
	}
	return &ProbeResult{Source: expect.SourceSQL, Response: toAnyList(rows)}, nil
}

// inboxQuery builds the canonical inbox query of SDD section 8.4 for the target
// the step names, and returns the connection, the query and its one parameter.
func inboxQuery(step *scenario.Step, rc *RunContext) (*sql.DB, string, any, error) {
	target := step.Target
	if target == "" {
		target = "app-db"
	}

	handle, ok := rc.DBs[target]
	if !ok {
		return nil, "", nil, fmt.Errorf("inbox/assert: no database connection for target %q", target)
	}

	dbTarget := resolveDBTarget(rc, target)
	if dbTarget == nil {
		return nil, "", nil, fmt.Errorf("inbox/assert: no database target config for %q", target)
	}
	if dbTarget.Inbox == nil {
		return nil, "", nil, fmt.Errorf("inbox/assert: target %q has no inbox config", target)
	}

	// The marker differs between dialects, and SQL Server rejects $1, so the
	// driver of the target decides it. SDD section 10.2 keeps a SQL Server
	// application database behind the cdc-sqlserver profile.
	dialect, err := citadeldb.DialectOf(dbTarget.Driver)
	if err != nil {
		return nil, "", nil, fmt.Errorf("inbox/assert: target %q: %w", target, err)
	}

	inbox := dbTarget.Inbox
	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s = %s",
		strings.Join(inboxColumns(inbox), ", "),
		inbox.Table,
		inbox.MessageIDColumn,
		dialect.Placeholder(1),
	)

	var messageID any
	if step.Where != nil {
		if id, exists := step.Where["messageId"]; exists {
			messageID = id
		}
	}
	if messageID == nil {
		return nil, "", nil, fmt.Errorf("inbox/assert: where.messageId is required")
	}

	return handle, query, messageID, nil
}

// inboxColumns returns the columns the canonical inbox query selects.
// Each name comes from the target configuration, because the standard names
// the concepts and each service names its own columns.
// An empty name falls back to its reference default, so a configuration that
// did not pass through config.Load still selects the full set.
func inboxColumns(inbox *config.InboxConfig) []string {
	pairs := [][2]string{
		{inbox.MessageIDColumn, config.DefaultInboxMessageIDColumn},
		{inbox.ProcessedAtColumn, config.DefaultInboxProcessedAtColumn},
		{inbox.RetryCountColumn, config.DefaultInboxRetryCountColumn},
		{inbox.LastErrorColumn, config.DefaultInboxLastErrorColumn},
	}

	columns := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair[0] != "" {
			columns = append(columns, pair[0])
			continue
		}
		columns = append(columns, pair[1])
	}
	return columns
}

func resolveDBTarget(rc *RunContext, target string) *config.DatabaseTarget {
	switch target {
	case "app-db":
		return rc.Config.Targets.AppDB
	case "upstream-shadow":
		return rc.Config.Targets.UpstreamShadow
	default:
		return nil
	}
}

func queryInboxRows(ctx context.Context, db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("execute query: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}

	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = values[i]
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	return result, nil
}
