package expect

import (
	"testing"

	"github.com/andersonten/simcitadel/internal/scenario"
)

func TestEvaluate_StatusMatch_Passes(t *testing.T) {
	exp := &scenario.Expect{Status: 200}
	r := Evaluate(exp, 200)
	if !r.Pass {
		t.Fatalf("expected pass, got: %s", r.Message)
	}
}

func TestEvaluate_StatusMismatch_Fails(t *testing.T) {
	exp := &scenario.Expect{Status: 200}
	r := Evaluate(exp, 404)
	if r.Pass {
		t.Fatal("expected failure for status mismatch")
	}
}

func TestEvaluate_JSONPathEquals_Passes(t *testing.T) {
	exp := &scenario.Expect{
		JSONPath: "$.name",
		Equals:   "alice",
	}
	response := map[string]any{"name": "alice"}
	r := Evaluate(exp, response)
	if !r.Pass {
		t.Fatalf("expected pass, got: %s", r.Message)
	}
}

func TestEvaluate_JSONPathEquals_TypeMismatch_Fails(t *testing.T) {
	exp := &scenario.Expect{
		JSONPath: "$.count",
		Equals:   "5",
	}
	response := map[string]any{"count": float64(5)}
	r := Evaluate(exp, response)
	if r.Pass {
		t.Fatal("expected failure for type mismatch (string vs number)")
	}
}

func TestEvaluate_JSONPathExists_Present_Passes(t *testing.T) {
	exp := &scenario.Expect{JSONPathExists: "$.data.id"}
	response := map[string]any{
		"data": map[string]any{"id": "abc"},
	}
	r := Evaluate(exp, response)
	if !r.Pass {
		t.Fatalf("expected pass, got: %s", r.Message)
	}
}

func TestEvaluate_JSONPathExists_Missing_Fails(t *testing.T) {
	exp := &scenario.Expect{JSONPathExists: "$.data.missing"}
	response := map[string]any{
		"data": map[string]any{"id": "abc"},
	}
	r := Evaluate(exp, response)
	if r.Pass {
		t.Fatal("expected failure for missing path")
	}
}

func TestEvaluate_Rows_Match_Passes(t *testing.T) {
	rows := 5
	exp := &scenario.Expect{Rows: &rows}
	r := Evaluate(exp, 5)
	if !r.Pass {
		t.Fatalf("expected pass, got: %s", r.Message)
	}
}

func TestEvaluate_Count_Match_Passes(t *testing.T) {
	count := 3
	exp := &scenario.Expect{Count: &count}
	r := Evaluate(exp, 3)
	if !r.Pass {
		t.Fatalf("expected pass, got: %s", r.Message)
	}
}

func TestEvaluate_CountAtLeast_Passes(t *testing.T) {
	min := 2
	exp := &scenario.Expect{CountAtLeast: &min}
	r := Evaluate(exp, 5)
	if !r.Pass {
		t.Fatalf("expected pass, got: %s", r.Message)
	}
}

func TestEvaluate_GraphQLErrors_Unset_DefaultsToNone(t *testing.T) {
	exp := &scenario.Expect{}
	// SDD section 6.5 scopes the default to the graphql source, so the response
	// names the source that produced it.
	response := Response{
		Source: SourceGraphQL,
		Body: map[string]any{
			"data": nil,
			"errors": []any{
				map[string]any{"message": "something failed"},
			},
		},
	}
	r := Evaluate(exp, response)
	if r.Pass {
		t.Fatal("expected fail when graphqlErrors defaults to none and errors are present")
	}
}

func TestEvaluate_GraphQLErrors_Unset_NonGraphQL_SkipsCheck(t *testing.T) {
	exp := &scenario.Expect{}
	response := map[string]any{
		"status": "ok",
	}
	r := Evaluate(exp, response)
	if !r.Pass {
		t.Fatalf("expected pass for non-GraphQL response, got: %s", r.Message)
	}
}

func TestEvaluate_GraphQLErrors_None_NoErrors_Passes(t *testing.T) {
	exp := &scenario.Expect{GraphQLErrors: "none"}
	response := map[string]any{
		"data": map[string]any{"user": "alice"},
	}
	r := Evaluate(exp, response)
	if !r.Pass {
		t.Fatalf("expected pass, got: %s", r.Message)
	}
}

func TestEvaluate_GraphQLErrors_None_WithErrors_Fails(t *testing.T) {
	exp := &scenario.Expect{GraphQLErrors: "none"}
	response := map[string]any{
		"data": nil,
		"errors": []any{
			map[string]any{"message": "something failed"},
		},
	}
	r := Evaluate(exp, response)
	if r.Pass {
		t.Fatal("expected failure when graphql errors are present")
	}
}
