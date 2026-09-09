package exampleapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Upstream reads the article service. In a Citadel environment WireMock answers
// it, which is the whole point of the platform: the app makes a real HTTP call
// with a real GraphQL body, and a stub answers it, so the app takes the code path
// it takes in production.
type Upstream struct {
	url    string
	client *http.Client
}

// NewUpstream creates a client against a GraphQL endpoint.
func NewUpstream(url string) *Upstream {
	return &Upstream{
		url: url,
		// A stub answers in milliseconds. The timeout exists so a missing stub
		// fails the step with a clear error rather than hanging the consumer, and
		// the scenario then fails on its await rather than on a run timeout.
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Article holds the fields the app uses. The upstream returns more, and ignoring
// the rest is deliberate: a scenario that adds a field to the stub must not need
// a change here.
type Article struct {
	Title       string
	Abstract    string
	JournalName string
}

// articleQuery names the field the WireMock mapping matches on. The reference
// mapping matches a body that contains articleReview_article, and it requires the
// ArticleLifecycle scenario to be in the ArticleSubmitted state, so a scenario
// that forgets its wiremock/set-state step gets no stub and this call fails.
//
// The field name follows the naming convention of the federated graph:
// PascalCase_entity for a curated domain API, rather than a raw database shape.
const articleQuery = `query ArticleReview_Article($id: String!) {
  articleReview_article(id: $id) {
    title
    abstract
    journal { id name }
  }
}`

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type articleResponse struct {
	Data struct {
		Article *struct {
			Title    string `json:"title"`
			Abstract string `json:"abstract"`
			Journal  *struct {
				Name string `json:"name"`
			} `json:"journal"`
		} `json:"articleReview_article"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Article reads one article. It reports an error when the upstream answers with a
// GraphQL error or with no article, because either one means the app cannot build
// its outbound event, and a silent empty title would reach the outbound event and
// look like a passing run.
func (u *Upstream) Article(ctx context.Context, articleID string) (*Article, error) {
	body, err := json.Marshal(graphqlRequest{
		Query:     articleQuery,
		Variables: map[string]any{"id": articleID},
	})
	if err != nil {
		return nil, fmt.Errorf("encode the upstream query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call the upstream at %s: %w", u.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the upstream at %s answered status %d, so no stub matched the query", u.url, resp.StatusCode)
	}

	var parsed articleResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode the upstream response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("the upstream reported an error: %s", parsed.Errors[0].Message)
	}
	if parsed.Data.Article == nil {
		return nil, fmt.Errorf("the upstream returned no article for id %s", articleID)
	}

	article := &Article{
		Title:    parsed.Data.Article.Title,
		Abstract: parsed.Data.Article.Abstract,
	}
	if parsed.Data.Article.Journal != nil {
		article.JournalName = parsed.Data.Article.Journal.Name
	}
	return article, nil
}
