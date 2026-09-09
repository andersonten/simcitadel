package expect

import (
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file holds the GraphQL error rules and the JSONPath filter typing
// rules of SDD section 6.5.

// TestEvaluate_GraphQLQueryError_ReportsTheQueryFailure holds the reason SDD
// section 6.5 calls the graphqlErrors default load-bearing. A query that errors
// returns 200 with a null data path, so a jsonPath that runs first reports
// "matched no nodes" and the developer reads a missing value in place of a
// failed query.
func TestEvaluate_GraphQLQueryError_ReportsTheQueryFailure(t *testing.T) {
	response := Response{
		Source:     SourceGraphQL,
		StatusCode: 200,
		Body: map[string]any{
			"data":   nil,
			"errors": []any{map[string]any{"message": "field pe_tasks not found"}},
		},
	}

	exp := &scenario.Expect{
		JSONPath: "$.data.pe_tasks.items[0].status",
		Equals:   "OPEN",
	}

	result := Evaluate(exp, response)
	if result.Pass {
		t.Fatal("a GraphQL error response passed a jsonPath expectation")
	}
	if !strings.Contains(result.Message, "graphqlErrors") {
		t.Errorf("message %q reports the missing path in place of the failed query", result.Message)
	}
	if !strings.Contains(result.Message, "field pe_tasks not found") {
		t.Errorf("message %q does not carry the error the server returned", result.Message)
	}
}

// TestEvaluate_GraphQLStatusFailure_ReportsTheStatus keeps the status key ahead
// of the error check, so a transport failure is not reported as a query
// failure.
func TestEvaluate_GraphQLStatusFailure_ReportsTheStatus(t *testing.T) {
	response := Response{
		Source:     SourceGraphQL,
		StatusCode: 503,
		Body: map[string]any{
			"errors": []any{map[string]any{"message": "upstream is down"}},
		},
	}

	result := Evaluate(&scenario.Expect{Status: 200}, response)
	if result.Pass {
		t.Fatal("a 503 passed a status 200 expectation")
	}
	if !strings.Contains(result.Message, "status") {
		t.Errorf("message %q does not report the status", result.Message)
	}
}

// TestEvalJSONPath_FilterOnANumberLiteral_RejectsAStringValue holds the
// comparison rule of SDD section 6.5 inside a filter. The section chooses strict
// type matching because loose matching hides an identifier that arrives as a
// string, and a filter selects the node the assertion then reads.
func TestEvalJSONPath_FilterOnANumberLiteral_RejectsAStringValue(t *testing.T) {
	doc := map[string]any{
		"data": map[string]any{
			"items": []any{
				map[string]any{"id": "515150", "status": "OPEN"},
			},
		},
	}

	values, err := EvalJSONPath(doc, "$.data.items[?(@.id==515150)].status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("a number literal matched the string id %q, so the assertion read %v",
			doc["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["id"], values)
	}
}

// TestEvalJSONPath_FilterOnAStringLiteral_RejectsANumberValue is the mirror
// case. A quoted literal is a string and must not match a JSON number.
func TestEvalJSONPath_FilterOnAStringLiteral_RejectsANumberValue(t *testing.T) {
	doc := map[string]any{
		"items": []any{
			map[string]any{"id": float64(515150), "status": "OPEN"},
		},
	}

	values, err := EvalJSONPath(doc, "$.items[?(@.id=='515150')].status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("a quoted literal matched the number id, so the assertion read %v", values)
	}
}

// TestEvalJSONPath_FilterMatchesItsOwnType holds the positive half, so the two
// tests above cannot pass because every filter matches nothing.
func TestEvalJSONPath_FilterMatchesItsOwnType(t *testing.T) {
	cases := []struct {
		name string
		doc  any
		path string
	}{
		{
			name: "string against a quoted literal",
			doc:  map[string]any{"items": []any{map[string]any{"taskKey": "assign-editor", "status": "OPEN"}}},
			path: "$.items[?(@.taskKey=='assign-editor')].status",
		},
		{
			name: "number against a bare literal",
			doc:  map[string]any{"items": []any{map[string]any{"id": float64(515150), "status": "OPEN"}}},
			path: "$.items[?(@.id==515150)].status",
		},
		{
			name: "number normalisation inside a filter",
			doc:  map[string]any{"items": []any{map[string]any{"id": float64(1), "status": "OPEN"}}},
			path: "$.items[?(@.id==1.0)].status",
		},
		{
			name: "boolean against a bare literal",
			doc:  map[string]any{"items": []any{map[string]any{"open": true, "status": "OPEN"}}},
			path: "$.items[?(@.open==true)].status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values, err := EvalJSONPath(tc.doc, tc.path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(values) != 1 || values[0] != "OPEN" {
				t.Fatalf("the filter did not select the node it names: %v", values)
			}
		})
	}
}
