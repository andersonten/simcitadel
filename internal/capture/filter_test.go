package capture

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFilter_Match_ExchangeFilter_MatchesExact(t *testing.T) {
	msg := Message{Exchange: "public.orchestration-engine.Article.v1"}
	f := Filter{Exchange: "public.orchestration-engine.Article.v1"}
	if !f.Match(msg) {
		t.Error("expected filter to match exact exchange")
	}

	f2 := Filter{Exchange: "other.exchange"}
	if f2.Match(msg) {
		t.Error("expected filter to reject non-matching exchange")
	}
}

func TestFilter_Match_EventTypeFilter_MatchesHeader(t *testing.T) {
	msg := Message{
		Headers: map[string]string{"eventType": "EditorAssigned"},
		Body:    json.RawMessage(`{"type": "SomethingElse"}`),
	}
	f := Filter{EventType: "EditorAssigned"}
	if !f.Match(msg) {
		t.Error("expected filter to match eventType from header")
	}

	f2 := Filter{EventType: "SomethingElse"}
	if f2.Match(msg) {
		t.Error("expected header eventType to take precedence over body type")
	}
}

func TestFilter_Match_EventTypeFilter_FallsBackToBody(t *testing.T) {
	msg := Message{
		Headers: map[string]string{},
		Body:    json.RawMessage(`{"type": "ArticleSubmitted", "specversion": "1.0"}`),
	}
	f := Filter{EventType: "ArticleSubmitted"}
	if !f.Match(msg) {
		t.Error("expected filter to match event type from body")
	}
}

func TestFilter_Match_RunIDFilter_MatchesExact(t *testing.T) {
	msg := Message{RunID: "run-abc-123"}
	f := Filter{RunID: "run-abc-123"}
	if !f.Match(msg) {
		t.Error("expected filter to match exact runId")
	}

	f2 := Filter{RunID: "run-other"}
	if f2.Match(msg) {
		t.Error("expected filter to reject non-matching runId")
	}
}

func TestFilter_Match_RoutingKeyPattern_AMQPSemantics(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		key     string
		want    bool
	}{
		{"star matches one word", "article.*", "article.created", true},
		{"star rejects two words", "article.*", "article.created.v1", false},
		{"hash matches zero words", "article.#", "article", true},
		{"hash matches one word", "article.#", "article.created", true},
		{"hash matches multiple words", "article.#", "article.created.v1.final", true},
		{"hash in middle", "article.#.final", "article.created.v1.final", true},
		{"exact match", "article.created", "article.created", true},
		{"exact mismatch", "article.created", "article.updated", false},
		{"star in middle", "article.*.v1", "article.created.v1", true},
		{"star in middle rejects", "article.*.v1", "article.created.v2", false},
		{"hash alone matches anything", "#", "any.routing.key", true},
		{"hash alone matches single", "#", "single", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Filter{RoutingKeyPattern: tt.pattern}
			msg := Message{RoutingKey: tt.key}
			got := f.Match(msg)
			if got != tt.want {
				t.Errorf("pattern %q vs key %q: got %v, want %v", tt.pattern, tt.key, got, tt.want)
			}
		})
	}
}

func TestFilter_Match_EventTypePattern_GlobSemantics(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		evType  string
		want    bool
	}{
		{"star suffix", "Editor*", "EditorAssigned", true},
		{"star prefix", "*Assigned", "EditorAssigned", true},
		{"star middle", "Editor*Event", "EditorAssignedEvent", true},
		{"no match", "Task*", "EditorAssigned", false},
		{"exact match", "EditorAssigned", "EditorAssigned", true},
		{"star matches empty", "Editor*", "Editor", true},
		{"double star", "*Article*", "NewArticleSubmitted", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Filter{EventTypePattern: tt.pattern}
			msg := Message{Headers: map[string]string{"eventType": tt.evType}}
			got := f.Match(msg)
			if got != tt.want {
				t.Errorf("pattern %q vs type %q: got %v, want %v", tt.pattern, tt.evType, got, tt.want)
			}
		})
	}
}

func TestFilter_Match_MultipleFilters_AllMustMatch(t *testing.T) {
	msg := Message{
		Exchange:   "exchange.a",
		RoutingKey: "article.created",
		RunID:      "run-1",
		Headers:    map[string]string{"eventType": "ArticleCreated"},
	}
	f := Filter{
		Exchange:  "exchange.a",
		RunID:     "run-1",
		EventType: "ArticleCreated",
	}
	if !f.Match(msg) {
		t.Error("expected all matching filters to pass")
	}

	f2 := Filter{
		Exchange:  "exchange.a",
		RunID:     "run-2",
		EventType: "ArticleCreated",
	}
	if f2.Match(msg) {
		t.Error("expected one non-matching filter to reject")
	}
}

func TestFilter_Match_HeaderFilter_MatchesExact(t *testing.T) {
	msg := Message{
		Headers: map[string]string{
			"version":   "1.0",
			"eventType": "Test",
		},
	}
	f := Filter{Headers: map[string]string{"version": "1.0"}}
	if !f.Match(msg) {
		t.Error("expected header filter to match")
	}

	f2 := Filter{Headers: map[string]string{"version": "2.0"}}
	if f2.Match(msg) {
		t.Error("expected header filter to reject non-matching value")
	}
}

func TestFilter_Match_JSONPath_MatchesExistingPath(t *testing.T) {
	msg := Message{
		Body: json.RawMessage(`{"type": "ArticleSubmitted", "data": {"id": "123"}}`),
	}
	f := Filter{JSONPath: "$.data.id"}
	if !f.Match(msg) {
		t.Error("expected jsonPath filter to match existing path")
	}
}

func TestFilter_Match_JSONPath_RejectsMissingPath(t *testing.T) {
	msg := Message{
		Body: json.RawMessage(`{"type": "ArticleSubmitted", "data": {"id": "123"}}`),
	}
	f := Filter{JSONPath: "$.data.missing"}
	if f.Match(msg) {
		t.Error("expected jsonPath filter to reject missing path")
	}
}

func TestFilter_Match_JSONPath_RejectsInvalidJSON(t *testing.T) {
	msg := Message{
		Body: json.RawMessage(`not valid json`),
	}
	f := Filter{JSONPath: "$.data"}
	if f.Match(msg) {
		t.Error("expected jsonPath filter to reject invalid JSON body")
	}
}

func TestFilter_Match_SinceFilter_FiltersOlder(t *testing.T) {
	now := time.Now()
	old := Message{ReceivedAt: now.Add(-10 * time.Minute)}
	recent := Message{ReceivedAt: now.Add(-1 * time.Minute)}

	f := Filter{Since: now.Add(-5 * time.Minute)}
	if f.Match(old) {
		t.Error("expected since filter to reject older message")
	}
	if !f.Match(recent) {
		t.Error("expected since filter to accept recent message")
	}
}
