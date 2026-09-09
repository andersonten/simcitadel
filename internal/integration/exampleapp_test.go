//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/capture"
	citadeldb "github.com/andersonten/simcitadel/internal/db"
	"github.com/andersonten/simcitadel/internal/exampleapp"
)

// The example app exists so the compose stack runs the whole pipeline, and its
// value depends on the outbox row being exactly what the Debezium EventRouter and
// the conformance check expect. Both are contracts in SQL and in JSON, so these
// tests run against a real Postgres.

const testOutboundExchange = "public.orchestration-engine.Manuscript.v1"

// newTestApp starts an app against a real database and a stub upstream.
func newTestApp(t *testing.T) (*exampleapp.App, *sql.DB) {
	t.Helper()
	ctx := context.Background()

	dsn := startPostgres(t)
	db, err := citadeldb.Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"articleReview_article":` +
			`{"title":"Simulated Article","abstract":"A","journal":{"name":"Frontiers"}}}}`))
	}))
	t.Cleanup(upstream.Close)

	app := exampleapp.New(exampleapp.Config{
		UpstreamGraphQLURL: upstream.URL,
		OutboundExchange:   testOutboundExchange,
	}, db, slog.Default())

	if err := app.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return app, db
}

func inboundEventJSON(id, articleID string) []byte {
	return []byte(fmt.Sprintf(`{
		"specversion":"1.0","type":"Article-Submitted","source":"Journal",
		"id":%q,"version":"v1.0.0","subject":"/articles/%s",
		"data":{"articleId":%s,"spaceId":"space-main"}
	}`, id, articleID, articleID))
}

// TestExampleApp_InboundEvent_WritesAnOutboxRowTheRouterCanRoute proves the
// outbox row satisfies SDD section 10.1 column by column. aggregatetype must hold
// the full exchange name, because the EventRouter routes on it, and type must hold
// the event type, because the transform places it in the eventType header that the
// capture service reads first.
func TestExampleApp_InboundEvent_WritesAnOutboxRowTheRouterCanRoute(t *testing.T) {
	app, db := newTestApp(t)
	ctx := context.Background()

	if err := app.Process(ctx, inboundEventJSON("evt-1", "515098")); err != nil {
		t.Fatalf("process the event: %v", err)
	}

	var aggregatetype, aggregateid, eventType string
	var payload []byte
	err := db.QueryRowContext(ctx,
		"SELECT aggregatetype, aggregateid, type, payload FROM public.outbox").
		Scan(&aggregatetype, &aggregateid, &eventType, &payload)
	if err != nil {
		t.Fatalf("read the outbox row: %v", err)
	}

	if aggregatetype != testOutboundExchange {
		t.Errorf("aggregatetype is %q, and the EventRouter needs the full exchange name %q",
			aggregatetype, testOutboundExchange)
	}
	if eventType != "Manuscript.Created" {
		t.Errorf("type is %q, and the reference scenario awaits Manuscript.Created", eventType)
	}
	if aggregateid == "" {
		t.Error("aggregateid is empty, so the aggregateId header carries nothing")
	}

	// The payload is the message body, because the transform runs with
	// expand.json.payload. The conformance check of section 4.11 fails a run on a
	// malformed envelope whether or not a scenario asserts it, so the app must
	// produce a conforming one.
	verdict := capture.ValidateCloudEvents(payload)
	if !verdict.Valid {
		t.Errorf("the outbox payload is not a conforming CloudEvents envelope: %v", verdict.Violations)
	}
}

// TestExampleApp_TheSameEventTwice_WritesOneOutboxRow proves the inbox does its
// job. A broker redelivers, and an app that writes a second outbox row publishes
// the same event twice.
func TestExampleApp_TheSameEventTwice_WritesOneOutboxRow(t *testing.T) {
	app, db := newTestApp(t)
	ctx := context.Background()

	if err := app.Process(ctx, inboundEventJSON("evt-dup", "515098")); err != nil {
		t.Fatalf("process the event: %v", err)
	}

	err := app.Process(ctx, inboundEventJSON("evt-dup", "515098"))
	if !errors.Is(err, exampleapp.ErrDuplicate) {
		t.Errorf("the second delivery reported %v, want ErrDuplicate", err)
	}

	var outboxRows, inboxRows int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public.outbox").Scan(&outboxRows); err != nil {
		t.Fatalf("count the outbox rows: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public.inbox").Scan(&inboxRows); err != nil {
		t.Fatalf("count the inbox rows: %v", err)
	}

	if outboxRows != 1 {
		t.Errorf("the app wrote %d outbox row(s) for one event, want 1", outboxRows)
	}
	if inboxRows != 1 {
		t.Errorf("the app wrote %d inbox row(s) for one event, want 1", inboxRows)
	}
}

// TestExampleApp_FailedEvent_IsProcessedOnRedelivery is the test that caught a
// real design defect. An inbox row means the app saw the event, and a non-null
// processed_at means it finished. A failure that recorded a row with a
// processed_at would make the event a permanent duplicate, so a redelivery would
// be dropped and the run would fail with no outbound event and no explanation.
func TestExampleApp_FailedEvent_IsProcessedOnRedelivery(t *testing.T) {
	ctx := context.Background()

	dsn := startPostgres(t)
	db, err := citadeldb.Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()

	// The upstream fails first and answers second, which is the shape of a
	// scenario that forgot its wiremock/set-state step and then set it.
	var healthy bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"articleReview_article":{"title":"T","journal":{"name":"J"}}}}`))
	}))
	defer upstream.Close()

	app := exampleapp.New(exampleapp.Config{
		UpstreamGraphQLURL: upstream.URL,
		OutboundExchange:   testOutboundExchange,
	}, db, slog.Default())
	if err := app.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	body := inboundEventJSON("evt-retry", "515098")

	if err := app.Process(ctx, body); err == nil {
		t.Fatal("the app processed an event whose upstream answered 404")
	}
	app.RecordFailure(ctx, body, fmt.Errorf("upstream answered 404"))

	// The redelivery must be processed, not dropped.
	healthy = true
	if err := app.Process(ctx, body); err != nil {
		t.Fatalf("the redelivery of a failed event was not processed: %v", err)
	}

	var outboxRows int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM public.outbox").Scan(&outboxRows); err != nil {
		t.Fatalf("count the outbox rows: %v", err)
	}
	if outboxRows != 1 {
		t.Errorf("the app wrote %d outbox row(s) after a failure and a retry, want 1", outboxRows)
	}

	// The failure is still visible, so inbox/assert can assert on it.
	var retryCount int
	var lastError sql.NullString
	err = db.QueryRowContext(ctx,
		"SELECT retry_count, last_error FROM public.inbox WHERE message_id = 'evt-retry'").
		Scan(&retryCount, &lastError)
	if err != nil {
		t.Fatalf("read the inbox row: %v", err)
	}
	if retryCount == 0 {
		t.Error("retry_count is 0 after a recorded failure, so inbox/assert cannot see it")
	}
	if !lastError.Valid || lastError.String == "" {
		t.Error("last_error is empty after a recorded failure")
	}
}

// TestExampleApp_GraphQL_AnswersFromApplicationState proves the app serves the
// endpoint that app.graphqlPath names, so an app/graphql step has something real
// to assert against.
func TestExampleApp_GraphQL_AnswersFromApplicationState(t *testing.T) {
	app, _ := newTestApp(t)
	ctx := context.Background()

	if err := app.Process(ctx, inboundEventJSON("evt-gql", "515098")); err != nil {
		t.Fatalf("process the event: %v", err)
	}

	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	query := `{"query":"query M($articleId: String!) { manuscriptByArticleId(articleId: $articleId) ` +
		`{ id articleId title status } }","variables":{"articleId":"515098"}}`
	resp, err := srv.Client().Post(srv.URL+"/graphql", "application/json", strings.NewReader(query))
	if err != nil {
		t.Fatalf("call the app graphql endpoint: %v", err)
	}
	defer resp.Body.Close()

	var parsed struct {
		Data struct {
			Manuscript *struct {
				ArticleID string `json:"articleId"`
				Title     string `json:"title"`
				Status    string `json:"status"`
			} `json:"manuscriptByArticleId"`
		} `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode the graphql response: %v", err)
	}
	if len(parsed.Errors) > 0 {
		t.Fatalf("the app reported a graphql error: %s", parsed.Errors[0].Message)
	}
	if parsed.Data.Manuscript == nil {
		t.Fatal("the app returned no manuscript for an article it processed")
	}
	if parsed.Data.Manuscript.ArticleID != "515098" {
		t.Errorf("the app returned article %q, want 515098", parsed.Data.Manuscript.ArticleID)
	}
	if parsed.Data.Manuscript.Status != "Created" {
		t.Errorf("the manuscript status is %q, want Created", parsed.Data.Manuscript.Status)
	}
}

// TestExampleApp_Health_IsDegradedUntilTheConsumerRuns proves Debezium can wait on
// this endpoint. Debezium resolves table.include.list when it starts and exits
// rather than wait, so a 200 before the outbox table exists would start it too
// early and it would exit 1.
func TestExampleApp_Health_IsDegradedUntilTheConsumerRuns(t *testing.T) {
	ctx := context.Background()

	dsn := startPostgres(t)
	db, err := citadeldb.Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()

	app := exampleapp.New(exampleapp.Config{OutboundExchange: testOutboundExchange}, db, slog.Default())
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	// Before the migration the app is not ready.
	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("probe the health endpoint: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("before the migration the app answers %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	// After the migration the database is ready and the consumer is not, so the
	// app is still not ready. That is the state Debezium must not start in.
	if err := app.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	resp, err = srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("probe the health endpoint: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("with no consumer the app answers %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}
