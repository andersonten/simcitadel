package exampleapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The upstream call is the reason WireMock is in the stack. A stub answers it, so
// the query the app sends must match what the reference mapping matches on, and a
// failure must be an error rather than an empty title that reaches the outbound
// event and looks like a pass.

// TestUpstreamArticle_Query_MatchesTheReferenceMapping proves the request the app
// sends satisfies the shipped WireMock mapping. That mapping matches a POST to
// /graphql whose body contains articleReview_article.
func TestUpstreamArticle_Query_MatchesTheReferenceMapping(t *testing.T) {
	var gotMethod, gotPath, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"data":{"articleReview_article":{"title":"T","abstract":"A","journal":{"name":"J"}}}}`))
	}))
	defer srv.Close()

	article, err := NewUpstream(srv.URL+"/graphql").Article(context.Background(), "515098")
	if err != nil {
		t.Fatalf("read the article: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("the app sent %s, and the mapping matches POST", gotMethod)
	}
	if gotPath != "/graphql" {
		t.Errorf("the app called %s, and the mapping matches /graphql", gotPath)
	}
	if !strings.Contains(gotBody, "articleReview_article") {
		t.Errorf("the body does not contain articleReview_article, so the reference mapping does not match it: %s", gotBody)
	}

	// The variable carries the id, so a mapping can match on it.
	var sent graphqlBody
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("the app sent a body that is not JSON: %v", err)
	}
	if sent.Variables["id"] != "515098" {
		t.Errorf("the app sent id %v, want 515098", sent.Variables["id"])
	}

	if article.Title != "T" || article.JournalName != "J" {
		t.Errorf("the app read title %q and journal %q", article.Title, article.JournalName)
	}
}

// TestUpstreamArticle_NoStubMatched_IsAnError covers the case a developer hits
// most: the scenario forgot its wiremock/set-state step, so no stub matches and
// WireMock answers 404.
func TestUpstreamArticle_NoStubMatched_IsAnError(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"wiremock matched no stub": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"the upstream reported a graphql error": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
		},
		"the upstream returned no article": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"articleReview_article":null}}`))
		},
		"the upstream returned a body that is not JSON": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<html>no</html>`))
		},
	}

	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()

			if _, err := NewUpstream(srv.URL).Article(context.Background(), "1"); err == nil {
				t.Error("the app read an article where the upstream supplied none")
			}
		})
	}
}
