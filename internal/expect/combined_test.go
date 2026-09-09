package expect

import (
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the expectation table of SDD section 6.5. One expect can
// name several keys, and every one of them must apply. These tests began as a
// proof that a status expectation hid every other key in the same block.

func intPtr(v int) *int { return &v }

// TestEvaluate_StatusWithJSONPath_AppliesBoth holds the combination case. A
// status that passes must not mask a jsonPath that fails.
func TestEvaluate_StatusWithJSONPath_AppliesBoth(t *testing.T) {
	response := Response{
		StatusCode: 200,
		Body: map[string]any{
			"data": map[string]any{"status": "CLOSED"},
		},
	}

	exp := &scenario.Expect{
		Status:   200,
		JSONPath: "$.data.status",
		Equals:   "OPEN",
	}

	result := Evaluate(exp, response)
	if result.Pass {
		t.Fatal("a passing status masked a failing jsonPath")
	}
	if !strings.Contains(result.Message, "OPEN") {
		t.Errorf("message %q does not report the jsonPath mismatch", result.Message)
	}
}

// TestEvaluate_StatusWithJSONPath_PassesWhenBothHold is the positive half, so
// the test above cannot pass for the wrong reason.
func TestEvaluate_StatusWithJSONPath_PassesWhenBothHold(t *testing.T) {
	response := Response{
		StatusCode: 200,
		Body: map[string]any{
			"data": map[string]any{"status": "OPEN"},
		},
	}

	exp := &scenario.Expect{
		Status:   200,
		JSONPath: "$.data.status",
		Equals:   "OPEN",
	}

	if result := Evaluate(exp, response); !result.Pass {
		t.Errorf("both expectations hold but the result failed: %s", result.Message)
	}
}

// TestEvaluate_WrongStatus_FailsEvenWhenTheBodyMatches keeps the status key
// meaningful once the body is also evaluated.
func TestEvaluate_WrongStatus_FailsEvenWhenTheBodyMatches(t *testing.T) {
	response := Response{
		StatusCode: 500,
		Body:       map[string]any{"data": map[string]any{"status": "OPEN"}},
	}

	exp := &scenario.Expect{Status: 200, JSONPath: "$.data.status", Equals: "OPEN"}

	result := Evaluate(exp, response)
	if result.Pass {
		t.Fatal("a 500 passed a status 200 expectation")
	}
	if !strings.Contains(result.Message, "status") {
		t.Errorf("message %q does not report the status mismatch", result.Message)
	}
}

// TestEvaluate_RowsWithColumnsAndNotNull_AppliesEveryKey holds the inbox
// assertion example of SDD section 8.4, which combines three keys.
func TestEvaluate_RowsWithColumnsAndNotNull_AppliesEveryKey(t *testing.T) {
	rows := []any{
		map[string]any{"retry_count": 0, "processed_at": "2026-08-19T09:00:00Z"},
	}

	pass := Evaluate(&scenario.Expect{
		Rows:    intPtr(1),
		Columns: map[string]any{"retry_count": 0},
		NotNull: []string{"processed_at"},
	}, Response{Body: rows})
	if !pass.Pass {
		t.Errorf("a conforming row failed: %s", pass.Message)
	}

	nullRows := []any{
		map[string]any{"retry_count": 0, "processed_at": nil},
	}
	fail := Evaluate(&scenario.Expect{
		Rows:    intPtr(1),
		Columns: map[string]any{"retry_count": 0},
		NotNull: []string{"processed_at"},
	}, Response{Body: nullRows})
	if fail.Pass {
		t.Error("a null processed_at passed a notNull expectation")
	}

	wrongColumn := Evaluate(&scenario.Expect{
		Rows:    intPtr(1),
		Columns: map[string]any{"retry_count": 3},
		NotNull: []string{"processed_at"},
	}, Response{Body: rows})
	if wrongColumn.Pass {
		t.Error("a retry_count of 0 passed an expectation of 3")
	}
}

// TestEvaluate_CountWithCloudEventsValid_AppliesBoth holds the captured-source
// combination, where the count reads the list and the envelope check reads its
// elements.
func TestEvaluate_CountWithCloudEventsValid_AppliesBoth(t *testing.T) {
	valid := map[string]any{
		"specversion": "1.0",
		"type":        "ArticleSubmitted",
		"source":      "SRV",
		"id":          "1",
		"version":     "v1",
	}
	messages := []any{map[string]any{"body": valid}}

	if r := Evaluate(&scenario.Expect{Count: intPtr(1), CloudEventsValid: true}, Response{Body: messages}); !r.Pass {
		t.Errorf("one valid message failed count plus cloudEventsValid: %s", r.Message)
	}

	if r := Evaluate(&scenario.Expect{Count: intPtr(2), CloudEventsValid: true}, Response{Body: messages}); r.Pass {
		t.Error("one message passed a count of 2")
	}

	invalid := []any{map[string]any{"body": map[string]any{"specversion": "1.0"}}}
	if r := Evaluate(&scenario.Expect{Count: intPtr(1), CloudEventsValid: true}, Response{Body: invalid}); r.Pass {
		t.Error("a message missing required attributes passed cloudEventsValid")
	}
}

// TestEvaluate_GraphQLErrors_DefaultToNone holds the load-bearing default of
// SDD section 6.5: a GraphQL error arrives with HTTP 200, so a 200 alone must
// not pass.
func TestEvaluate_GraphQLErrors_DefaultToNone(t *testing.T) {
	// SDD section 6.5 scopes the default to the graphql source, so the response
	// names the source that produced it.
	response := Response{
		Source:     SourceGraphQL,
		StatusCode: 200,
		Body: map[string]any{
			"data":   nil,
			"errors": []any{map[string]any{"message": "field pe_tasks not found"}},
		},
	}

	result := Evaluate(&scenario.Expect{Status: 200}, response)
	if result.Pass {
		t.Fatal("a GraphQL error response passed a status 200 expectation")
	}
	if !strings.Contains(result.Message, "graphqlErrors") {
		t.Errorf("message %q does not name the GraphQL errors", result.Message)
	}
}
