package expect

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// Result holds the outcome of evaluating an expectation.
type Result struct {
	Pass     bool
	Expected any
	Actual   any
	Message  string
}

// Response carries everything one probe produced. SDD section 6.5 lets a single
// expect combine keys that read different parts of the outcome, for example a
// status and a jsonPath, so the evaluator needs all of them at once rather than
// one collapsed value.
type Response struct {
	// Source names the probe source of SDD section 6.5 that produced the
	// response. An expectation key that the table scopes to one source, such as
	// graphqlErrors, reads it rather than guessing from the shape of the body.
	Source string
	// StatusCode is the HTTP status, or 0 for a source that has none.
	StatusCode int
	// Body is the parsed payload: a JSON document, a row map, or a match list.
	Body any
	// Count is the match count for a captured or journal query. It is nil when
	// the source does not produce one.
	Count *int
}

// The probe sources of SDD section 6.5.
const (
	SourceGraphQL  = "graphql"
	SourceHTTP     = "http"
	SourceSQL      = "sql"
	SourceCaptured = "captured"
	SourceJournal  = "journal"
)

// Evaluate checks an expectation against a response.
// The response can be a Response, an HTTP response body (as parsed JSON), SQL
// rows, or a captured message list.
func Evaluate(exp *scenario.Expect, response any) Result {
	if exp == nil {
		return Result{Pass: true, Message: "no expectation defined"}
	}

	if exp.Status != 0 {
		r := evaluateStatus(exp.Status, response)
		if !r.Pass {
			return r
		}
	}

	// SDD section 6.5 scopes graphqlErrors to the graphql source, so the default
	// follows the source and not the shape of the body. A REST body that carries
	// a data key or an errors key is a REST body.
	//
	// The check runs before every key that reads the body, which is the reason
	// section 6.5 calls the default load-bearing. A query that errors returns
	// 200 with a null data path, so a jsonPath that runs first reports a missing
	// value in place of the failed query.
	graphqlErrors := exp.GraphQLErrors
	if graphqlErrors == "" && responseSource(response) == SourceGraphQL {
		graphqlErrors = "none"
	}
	if graphqlErrors != "" {
		r := evaluateGraphQLErrors(graphqlErrors, response)
		if !r.Pass {
			return r
		}
	}

	if exp.JSONPathExists != "" {
		r := evaluateJSONPathExists(exp.JSONPathExists, response)
		if !r.Pass {
			return r
		}
	}

	if exp.JSONPath != nil && exp.Equals != nil {
		r := evaluateJSONPathEquals(exp.JSONPath, exp.Equals, response)
		if !r.Pass {
			return r
		}
	} else if exp.JSONPath != nil {
		r := evaluateJSONPathMap(exp.JSONPath, response)
		if !r.Pass {
			return r
		}
	}

	if exp.Rows != nil {
		r := evaluateRows(*exp.Rows, response)
		if !r.Pass {
			return r
		}
	}

	if exp.Count != nil {
		r := evaluateCount(*exp.Count, response)
		if !r.Pass {
			return r
		}
	}

	if exp.CountAtLeast != nil {
		r := evaluateCountAtLeast(*exp.CountAtLeast, response)
		if !r.Pass {
			return r
		}
	}

	if exp.Columns != nil {
		r := evaluateColumns(exp.Columns, response)
		if !r.Pass {
			return r
		}
	}

	if len(exp.NotNull) > 0 {
		r := evaluateNotNull(exp.NotNull, response)
		if !r.Pass {
			return r
		}
	}

	if exp.CloudEventsValid {
		r := evaluateCloudEventsValid(response)
		if !r.Pass {
			return r
		}
	}

	return Result{Pass: true, Message: "all expectations pass"}
}

func evaluateStatus(expected int, response any) Result {
	actual, ok := extractStatusCode(response)
	if !ok {
		return Result{
			Pass:     false,
			Expected: expected,
			Actual:   nil,
			Message:  "response does not contain a status code",
		}
	}
	if actual != expected {
		return Result{
			Pass:     false,
			Expected: expected,
			Actual:   actual,
			Message:  fmt.Sprintf("status: expected %d, got %d", expected, actual),
		}
	}
	return Result{Pass: true, Expected: expected, Actual: actual}
}

func extractStatusCode(response any) (int, bool) {
	switch v := response.(type) {
	case Response:
		if v.StatusCode == 0 {
			return 0, false
		}
		return v.StatusCode, true
	case int:
		return v, true
	case float64:
		return int(v), true
	case map[string]any:
		if sc, ok := v["statusCode"]; ok {
			return toInt(sc)
		}
	}
	return 0, false
}

// extractCount returns the match count a count expectation reads. A source that
// reports its own count wins; otherwise the length of a list body is used.
func extractCount(response any) (int, bool) {
	if r, ok := response.(Response); ok {
		if r.Count != nil {
			return *r.Count, true
		}
		if arr, isList := r.Body.([]any); isList {
			return len(arr), true
		}
		return toInt(r.Body)
	}
	if arr, ok := response.([]any); ok {
		return len(arr), true
	}
	return toInt(response)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	case int64:
		return int(n), true
	}
	return 0, false
}

func evaluateJSONPathEquals(path any, equals any, response any) Result {
	pathStr, ok := path.(string)
	if !ok {
		return Result{Pass: false, Message: "jsonPath must be a string when used with equals"}
	}
	body := extractBody(response)
	values, err := EvalJSONPath(body, pathStr)
	if err != nil {
		return Result{Pass: false, Expected: equals, Message: fmt.Sprintf("jsonpath error: %v", err)}
	}
	if len(values) == 0 {
		return Result{Pass: false, Expected: equals, Actual: nil, Message: fmt.Sprintf("jsonpath %s matched no nodes", pathStr)}
	}
	actual := values[0]
	if !CompareValues(equals, actual) {
		return Result{
			Pass:     false,
			Expected: equals,
			Actual:   actual,
			Message:  fmt.Sprintf("jsonpath %s: expected %v, got %v", pathStr, equals, actual),
		}
	}
	return Result{Pass: true, Expected: equals, Actual: actual}
}

func evaluateJSONPathMap(jsonPath any, response any) Result {
	m, ok := jsonPath.(map[string]any)
	if !ok {
		return Result{Pass: false, Message: "jsonPath must be a string or a map"}
	}
	body := extractBody(response)
	for path, expected := range m {
		values, err := EvalJSONPath(body, path)
		if err != nil {
			return Result{Pass: false, Expected: expected, Message: fmt.Sprintf("jsonpath error for %s: %v", path, err)}
		}
		if len(values) == 0 {
			return Result{Pass: false, Expected: expected, Actual: nil, Message: fmt.Sprintf("jsonpath %s matched no nodes", path)}
		}
		if !CompareValues(expected, values[0]) {
			return Result{
				Pass:     false,
				Expected: expected,
				Actual:   values[0],
				Message:  fmt.Sprintf("jsonpath %s: expected %v, got %v", path, expected, values[0]),
			}
		}
	}
	return Result{Pass: true, Message: "all jsonpath assertions pass"}
}

func evaluateJSONPathExists(path string, response any) Result {
	body := extractBody(response)
	values, err := EvalJSONPath(body, path)
	if err != nil {
		return Result{Pass: false, Message: fmt.Sprintf("jsonpathExists error: %v", err)}
	}
	if len(values) == 0 {
		return Result{
			Pass:    false,
			Actual:  nil,
			Message: fmt.Sprintf("jsonpathExists: %s matched no nodes", path),
		}
	}
	return Result{Pass: true, Actual: values[0]}
}

func evaluateRows(expected int, response any) Result {
	actual, ok := extractCount(response)
	if !ok {
		return Result{Pass: false, Expected: expected, Actual: response, Message: "rows: response is not an integer"}
	}
	if actual != expected {
		return Result{
			Pass:     false,
			Expected: expected,
			Actual:   actual,
			Message:  fmt.Sprintf("rows: expected %d, got %d", expected, actual),
		}
	}
	return Result{Pass: true, Expected: expected, Actual: actual}
}

func evaluateCount(expected int, response any) Result {
	actual, ok := extractCount(response)
	if !ok {
		return Result{Pass: false, Expected: expected, Actual: response, Message: "count: response is not an integer"}
	}
	if actual != expected {
		return Result{
			Pass:     false,
			Expected: expected,
			Actual:   actual,
			Message:  fmt.Sprintf("count: expected %d, got %d", expected, actual),
		}
	}
	return Result{Pass: true, Expected: expected, Actual: actual}
}

func evaluateCountAtLeast(minimum int, response any) Result {
	actual, ok := extractCount(response)
	if !ok {
		return Result{Pass: false, Expected: minimum, Actual: response, Message: "countAtLeast: response is not an integer"}
	}
	if actual < minimum {
		return Result{
			Pass:     false,
			Expected: minimum,
			Actual:   actual,
			Message:  fmt.Sprintf("countAtLeast: expected at least %d, got %d", minimum, actual),
		}
	}
	return Result{Pass: true, Expected: minimum, Actual: actual}
}

func evaluateColumns(expected map[string]any, response any) Result {
	row, ok := firstRow(extractBody(response))
	if !ok {
		return Result{Pass: false, Expected: expected, Actual: response, Message: "columns: response is not a row map"}
	}
	for col, exp := range expected {
		actual, exists := row[col]
		if !exists {
			return Result{
				Pass:     false,
				Expected: exp,
				Actual:   nil,
				Message:  fmt.Sprintf("columns: column %q not found in row", col),
			}
		}
		if !CompareValues(exp, actual) {
			return Result{
				Pass:     false,
				Expected: exp,
				Actual:   actual,
				Message:  fmt.Sprintf("columns: %s expected %v, got %v", col, exp, actual),
			}
		}
	}
	return Result{Pass: true, Expected: expected, Actual: response}
}

// firstRow returns the row a column expectation reads. SDD section 6.5 scopes
// columns and notNull to the first row.
func firstRow(body any) (map[string]any, bool) {
	switch v := body.(type) {
	case map[string]any:
		return v, true
	case []any:
		if len(v) == 0 {
			return nil, false
		}
		row, ok := v[0].(map[string]any)
		return row, ok
	}
	return nil, false
}

func evaluateNotNull(columns []string, response any) Result {
	row, ok := firstRow(extractBody(response))
	if !ok {
		return Result{Pass: false, Message: "notNull: response is not a row map"}
	}
	for _, col := range columns {
		v, exists := row[col]
		if !exists || v == nil {
			return Result{
				Pass:    false,
				Actual:  nil,
				Message: fmt.Sprintf("notNull: column %q is null or missing", col),
			}
		}
	}
	return Result{Pass: true, Message: "all notNull columns have values"}
}

func evaluateCloudEventsValid(response any) Result {
	messages, ok := extractBody(response).([]any)
	if !ok {
		return Result{Pass: false, Message: "cloudEventsValid: response is not a list of messages"}
	}
	for i, msg := range messages {
		raw, err := json.Marshal(msg)
		if err != nil {
			return Result{Pass: false, Message: fmt.Sprintf("cloudEventsValid: message %d cannot be serialized", i)}
		}
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			return Result{Pass: false, Message: fmt.Sprintf("cloudEventsValid: message %d is not a JSON object", i)}
		}
		if err := ValidateCloudEvent(CloudEventEnvelope(entry)); err != nil {
			return Result{Pass: false, Message: fmt.Sprintf("cloudEventsValid: message %d: %v", i, err)}
		}
	}
	return Result{Pass: true, Message: "all messages are valid CloudEvents"}
}

// CloudEventEnvelope returns the CloudEvents envelope carried by a capture
// service entry. The capture entry of SDD section 8.2 wraps the envelope under
// "body", so a caller that holds either shape gets the envelope itself.
func CloudEventEnvelope(entry map[string]any) map[string]any {
	if body, ok := entry["body"].(map[string]any); ok {
		return body
	}
	return entry
}

// requiredCloudEventAttributes lists the CloudEvents 1.0 attributes that SDD
// section 4.11 requires, plus the required version attribute.
var requiredCloudEventAttributes = []string{"specversion", "type", "source", "id", "version"}

// ValidateCloudEvent checks an envelope against CloudEvents 1.0 and the
// version attribute. ValidateCloudEvent returns nil when the envelope conforms.
func ValidateCloudEvent(envelope map[string]any) error {
	for _, attr := range requiredCloudEventAttributes {
		v, ok := envelope[attr]
		if !ok {
			return fmt.Errorf("missing required attribute %q", attr)
		}
		s, ok := v.(string)
		if !ok || s == "" {
			return fmt.Errorf("attribute %q must be a non-empty string", attr)
		}
	}
	if s, ok := envelope["specversion"].(string); ok && s != "1.0" {
		return fmt.Errorf("specversion must be \"1.0\", got %q", s)
	}
	return nil
}

func evaluateGraphQLErrors(setting string, response any) Result {
	if setting == "any" {
		return Result{Pass: true, Message: "graphqlErrors: errors are allowed"}
	}
	body, ok := extractBody(response).(map[string]any)
	if !ok {
		return Result{Pass: true}
	}
	errors, exists := body["errors"]
	if !exists {
		return Result{Pass: true}
	}
	arr, ok := errors.([]any)
	if !ok {
		return Result{Pass: true}
	}
	if len(arr) > 0 {
		return Result{
			Pass:    false,
			Actual:  arr,
			Message: fmt.Sprintf("graphqlErrors: expected none but the query failed: %s", graphQLErrorText(arr)),
		}
	}
	return Result{Pass: true}
}

// graphQLErrorText joins the message of each entry in a GraphQL errors array.
// SDD section 6.5 wants the failure to read as "the query failed", so the
// message the server returned belongs in it.
func graphQLErrorText(arr []any) string {
	texts := make([]string, 0, len(arr))
	for _, entry := range arr {
		m, ok := entry.(map[string]any)
		if !ok {
			texts = append(texts, fmt.Sprintf("%v", entry))
			continue
		}
		if message, isString := m["message"].(string); isString {
			texts = append(texts, message)
			continue
		}
		texts = append(texts, fmt.Sprintf("%v", entry))
	}
	return strings.Join(texts, "; ")
}

// responseSource returns the probe source that produced the response. It
// returns an empty string for a caller that hands over a bare body, which
// carries no source of its own.
func responseSource(response any) string {
	if r, ok := response.(Response); ok {
		return r.Source
	}
	return ""
}

func extractBody(response any) any {
	if r, ok := response.(Response); ok {
		return r.Body
	}
	if m, ok := response.(map[string]any); ok {
		if body, exists := m["body"]; exists {
			return body
		}
	}
	return response
}
