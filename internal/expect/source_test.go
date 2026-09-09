package expect

import (
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the scope of graphqlErrors in SDD section 6.5, where the
// key applies to the graphql source only. These tests began as a proof that the
// default keyed off the response shape: a REST body that carried a data key got
// a GraphQL error check it never asked for.

// TestEvaluate_RESTResponseWithDataAndErrors_SkipsTheGraphQLDefault holds the
// source column of the expectation table in SDD section 6.5.
func TestEvaluate_RESTResponseWithDataAndErrors_SkipsTheGraphQLDefault(t *testing.T) {
	response := Response{
		Source:     SourceHTTP,
		StatusCode: 200,
		Body: map[string]any{
			"data":   map[string]any{"articleId": 515098},
			"errors": []any{map[string]any{"message": "the REST API reports its own errors list"}},
		},
	}

	result := Evaluate(&scenario.Expect{Status: 200}, response)
	if !result.Pass {
		t.Fatalf("a REST response failed a GraphQL error check it never asked for: %s", result.Message)
	}
}

// TestEvaluate_GraphQLResponseWithErrors_FailsWithoutAnExplicitSetting holds the
// load-bearing default of SDD section 6.5 for the source it belongs to.
func TestEvaluate_GraphQLResponseWithErrors_FailsWithoutAnExplicitSetting(t *testing.T) {
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

// TestEvaluate_GraphQLResponseWithNeitherKey_StillChecks keeps the default on
// the source rather than on the shape of the body.
func TestEvaluate_GraphQLResponseWithNeitherKey_StillChecks(t *testing.T) {
	response := Response{
		Source:     SourceGraphQL,
		StatusCode: 200,
		Body:       map[string]any{"pe_tasks": []any{}},
	}

	result := Evaluate(&scenario.Expect{Status: 200}, response)
	if !result.Pass {
		t.Fatalf("a GraphQL response with no errors key failed: %s", result.Message)
	}
}

// TestEvaluate_GraphQLErrorsAny_AllowsErrors keeps the explicit setting of SDD
// section 6.5 able to override the default.
func TestEvaluate_GraphQLErrorsAny_AllowsErrors(t *testing.T) {
	response := Response{
		Source:     SourceGraphQL,
		StatusCode: 200,
		Body: map[string]any{
			"errors": []any{map[string]any{"message": "the scenario expects this error"}},
		},
	}

	result := Evaluate(&scenario.Expect{Status: 200, GraphQLErrors: "any"}, response)
	if !result.Pass {
		t.Fatalf("graphqlErrors: any rejected an error response: %s", result.Message)
	}
}
