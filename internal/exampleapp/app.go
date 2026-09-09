package exampleapp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// newUUID returns a UUIDv4 string. internal/scenario builds one the same way
// rather than take a dependency for it, and CONTRIBUTING.md asks for the standard
// library first, so the app follows that rather than promote google/uuid to a
// direct dependency.
func newUUID() string {
	var b [16]byte
	// crypto/rand.Read fills the buffer or panics inside the standard library, so
	// there is no error path to handle here.
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Config holds everything the app needs. The caller reads it, so no package
// under internal reads the environment.
type Config struct {
	// AMQPURL addresses the broker the app consumes from and the outbox reaches
	// through Debezium.
	AMQPURL string

	// InboundExchange is the exchange the legacy publisher writes to. The
	// reference scenario publishes Journal.Article-Submitted.1.0.0 to it.
	InboundExchange string

	// InboundQueue is the queue this app owns on that exchange. A named durable
	// queue keeps the binding across a restart, so an event published while the
	// app restarts is still delivered.
	InboundQueue string

	// InboundRoutingKey binds the queue. It is a topic pattern.
	InboundRoutingKey string

	// InboundAdditionalRoutingKeys holds extra topic patterns that bind to the
	// same queue. The closed-loop scenario uses this to receive the Task-Assigned
	// event that WireMock fires through the event-trigger.
	InboundAdditionalRoutingKeys []string

	// UpstreamGraphQLURL is the GraphQL endpoint of the upstream the app reads.
	// In a Citadel environment that is WireMock, which is the point: the app
	// makes a real call and a stub answers it.
	UpstreamGraphQLURL string

	// OutboundExchange is the value the app writes to outbox.aggregatetype. SDD
	// section 10.1 makes that column the full exchange name.
	OutboundExchange string

	// ListenAddr is the address of the HTTP server.
	ListenAddr string
}

// App is the example application. It consumes an inbound event, reads an
// upstream, and writes an inbox row and an outbox row in one transaction.
type App struct {
	cfg      Config
	db       *sql.DB
	upstream *Upstream
	logger   *slog.Logger

	// mu guards the two readiness flags. The health endpoint reads them from the
	// HTTP goroutine while the consumer writes them.
	mu       sync.RWMutex
	dbReady  bool
	amqpBusy bool

	// processed counts the events the app turned into an outbox row. The health
	// body reports it, which makes a stuck consumer visible without a database
	// client.
	processed int
}

// New creates an app. It opens no connection, so the caller controls the order.
func New(cfg Config, db *sql.DB, logger *slog.Logger) *App {
	return &App{
		cfg:      cfg,
		db:       db,
		upstream: NewUpstream(cfg.UpstreamGraphQLURL),
		logger:   logger,
	}
}

// inboundEvent is the envelope the app consumes. It reads the attributes it needs
// and keeps the rest, because an unknown attribute is not an error.
type inboundEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Source  string `json:"source"`
	Subject string `json:"subject"`
	Data    struct {
		// ArticleID arrives as a JSON number in the reference payload, and a
		// number is what the upstream query takes. json.Number accepts either
		// shape, so a publisher that quotes it does not break the app.
		ArticleID json.Number `json:"articleId"`
		SpaceID   string      `json:"spaceId"`
	} `json:"data"`
}

// outboundEvent is the CloudEvents envelope the app writes to the outbox. The
// five attributes the conformance check of SDD section 4.11 requires are all
// present and all strings, because a run fails on a missing one whether or not a
// scenario asserts it.
type outboundEvent struct {
	SpecVersion string         `json:"specversion"`
	Type        string         `json:"type"`
	Source      string         `json:"source"`
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	Subject     string         `json:"subject"`
	Time        string         `json:"time"`
	Data        map[string]any `json:"data"`
}

// ErrDuplicate reports an event the app already processed. The caller
// acknowledges the delivery, because a redelivery of a processed event is a
// success and not a failure.
var ErrDuplicate = fmt.Errorf("event already processed")

// Process turns one inbound event into one outbound event. It reads the upstream
// first, then writes the inbox row and the outbox row in one transaction.
//
// The order is the point of the exercise. The upstream read happens outside the
// transaction, because a stub that is slow must not hold a database lock. The
// inbox row and the outbox row commit together, so the app never records an event
// as processed without producing its output, and never produces its output twice.
func (a *App) Process(ctx context.Context, body []byte) error {
	var in inboundEvent
	if err := json.Unmarshal(body, &in); err != nil {
		return fmt.Errorf("parse the inbound envelope: %w", err)
	}
	if in.ID == "" {
		return fmt.Errorf("the inbound envelope carries no id, so the inbox cannot hold it")
	}

	// A duplicate is cheap to detect and expensive to process, so the app checks
	// before it calls the upstream. The transaction below repeats the check,
	// because two deliveries can reach this line at once.
	duplicate, err := a.alreadyProcessed(ctx, in.ID)
	if err != nil {
		return err
	}
	if duplicate {
		return ErrDuplicate
	}

	if in.Type == eventTypeTaskAssigned {
		return a.processTaskAssigned(ctx, in)
	}

	article, err := a.upstream.Article(ctx, in.Data.ArticleID.String())
	if err != nil {
		return fmt.Errorf("read the upstream article: %w", err)
	}

	return a.commit(ctx, in, article)
}

// processTaskAssigned handles a Task-Assigned event that arrives through the
// closed loop. WireMock fires it via postServeAction when the app queries the
// federated gateway. The app updates the manuscript status and writes an outbox
// row so the scenario can assert the full chain terminated.
func (a *App) processTaskAssigned(ctx context.Context, in inboundEvent) error {
	articleID := in.Data.ArticleID.String()

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO public.inbox (message_id, processed_at) VALUES ($1, now())
		ON CONFLICT (message_id) DO UPDATE SET processed_at = now()
		WHERE public.inbox.processed_at IS NULL`, in.ID)
	if err != nil {
		return fmt.Errorf("write the inbox row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read the inbox insert result: %w", err)
	}
	if affected == 0 {
		return ErrDuplicate
	}

	var manuscriptID string
	err = tx.QueryRowContext(ctx, `
		UPDATE public.manuscript SET status = 'Assigned'
		WHERE article_id = $1
		RETURNING id`, articleID).Scan(&manuscriptID)
	if err != nil {
		return fmt.Errorf("update the manuscript status: %w", err)
	}

	event := outboundEvent{
		SpecVersion: "1.0",
		Type:        eventTypeManuscriptAssigned,
		Source:      "ExampleApp",
		ID:          newUUID(),
		Version:     "v1.0.0",
		Subject:     "/manuscripts/" + manuscriptID,
		Time:        time.Now().UTC().Format(time.RFC3339Nano),
		Data: map[string]any{
			"manuscriptId": manuscriptID,
			"articleId":    articleID,
			"assignedBy":   in.Source,
			"causedBy":     in.ID,
		},
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode the outbound envelope: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO public.outbox (id, aggregatetype, aggregateid, type, payload)
		VALUES ($1, $2, $3, $4, $5)`,
		newUUID(), a.cfg.OutboundExchange, manuscriptID, eventTypeManuscriptAssigned, payload)
	if err != nil {
		return fmt.Errorf("write the outbox row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the transaction: %w", err)
	}

	a.mu.Lock()
	a.processed++
	a.mu.Unlock()

	a.logger.Info("task-assigned processed",
		"inboundId", in.ID,
		"manuscriptId", manuscriptID,
		"articleId", articleID,
		"outboundType", eventTypeManuscriptAssigned)
	return nil
}

// alreadyProcessed reads the inbox. A row with a null processed_at is an event the
// app saw and failed, so it is not a duplicate and the caller processes it again.
func (a *App) alreadyProcessed(ctx context.Context, messageID string) (bool, error) {
	var exists bool
	err := a.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM public.inbox WHERE message_id = $1 AND processed_at IS NOT NULL)",
		messageID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read the inbox: %w", err)
	}
	return exists, nil
}

// commit writes the inbox row, the manuscript row and the outbox row together.
func (a *App) commit(ctx context.Context, in inboundEvent, article *Article) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The insert is the lock. Two concurrent deliveries of one event both reach
	// here, and the row lock means exactly one of them sets processed_at, so
	// exactly one writes an outbox row.
	//
	// The conditional update is what makes a retry work. A row that a failure left
	// behind has a null processed_at, so this claims it. A row that a success left
	// behind does not match the WHERE, so RowsAffected is zero and the caller
	// treats the delivery as a duplicate.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO public.inbox (message_id, processed_at) VALUES ($1, now())
		ON CONFLICT (message_id) DO UPDATE SET processed_at = now()
		WHERE public.inbox.processed_at IS NULL`, in.ID)
	if err != nil {
		return fmt.Errorf("write the inbox row: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read the inbox insert result: %w", err)
	}
	if affected == 0 {
		return ErrDuplicate
	}

	articleID := in.Data.ArticleID.String()
	manuscriptID := newUUID()

	// The manuscript row is the app's own state, and it is what the GraphQL
	// endpoint answers from. A second event for one article updates the row it
	// already has, so the article id stays unique.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO public.manuscript (id, article_id, title, status)
		VALUES ($1, $2, $3, 'Created')
		ON CONFLICT (article_id) DO UPDATE SET title = EXCLUDED.title
		RETURNING id`,
		manuscriptID, articleID, article.Title).Scan(&manuscriptID)
	if err != nil {
		return fmt.Errorf("write the manuscript row: %w", err)
	}

	event := outboundEvent{
		SpecVersion: "1.0",
		Type:        eventTypeManuscriptCreated,
		Source:      "ExampleApp",
		ID:          newUUID(),
		Version:     "v1.0.0",
		Subject:     "/manuscripts/" + manuscriptID,
		Time:        time.Now().UTC().Format(time.RFC3339Nano),
		Data: map[string]any{
			"manuscriptId": manuscriptID,
			"articleId":    articleID,
			"spaceId":      in.Data.SpaceID,
			"title":        article.Title,
			"journal":      article.JournalName,
			"causedBy":     in.ID,
		},
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode the outbound envelope: %w", err)
	}

	// aggregatetype carries the full exchange name, per SDD section 10.1, and the
	// EventRouter routes on it. type reaches the broker as the eventType header
	// through table.fields.additional.placement, and the capture service reads
	// that header first.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO public.outbox (id, aggregatetype, aggregateid, type, payload)
		VALUES ($1, $2, $3, $4, $5)`,
		newUUID(), a.cfg.OutboundExchange, manuscriptID, eventTypeManuscriptCreated, payload)
	if err != nil {
		return fmt.Errorf("write the outbox row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the transaction: %w", err)
	}

	a.mu.Lock()
	a.processed++
	a.mu.Unlock()

	a.logger.Info("event processed",
		"inboundId", in.ID,
		"inboundType", in.Type,
		"manuscriptId", manuscriptID,
		"articleId", articleID,
		"outboundExchange", a.cfg.OutboundExchange,
		"outboundType", eventTypeManuscriptCreated)
	return nil
}

// eventTypeManuscriptCreated is the outbound event type. The reference scenario
// awaits it by name, so it is a constant and not a formatted string.
const eventTypeManuscriptCreated = "Manuscript.Created"

// eventTypeManuscriptAssigned is the outbound event the app writes when it
// processes a Task-Assigned event from the closed loop.
const eventTypeManuscriptAssigned = "Manuscript.Assigned"

// eventTypeTaskAssigned is the inbound event type that the closed-loop stub
// fires through the event-trigger. The app matches on it to route the delivery
// to processTaskAssigned rather than the article-submitted path.
const eventTypeTaskAssigned = "Task-Assigned"
